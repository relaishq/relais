// Package framecache stores complete encoded video frames outside workers.
// Each track retains only its current keyframe group; no reader uses a previous
// group. Whole groups are dropped on overflow or missing references. Memory
// copies complete packetizations and evicts the oldest sessions globally.
package framecache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// Track identifies the outbound stream whose source packets are cached.
type Track struct {
	Kind string
	SSRC uint32
}

// Packet preserves source sequence numbers and packetization. Payload includes
// the codec's RTP descriptor; Marker ends the frame.
type Packet struct {
	SequenceNumber uint16
	Marker         bool
	Payload        []byte
}

// Frame contains every source packet from a frame's start through its marker.
// Timestamp is the source RTP timestamp; Arrival is local receipt time.
type Frame struct {
	Track     Track
	Timestamp uint32
	// SourceSSRC and EchoTimestamp let a stale, even unanchored, snapshot
	// recover the source identity and the last observed outbound RTP clock.
	SourceSSRC    uint32
	EchoTimestamp uint32
	Arrival       time.Time
	// Redis supplies elapsed server time; local monotonic time advances it after read.
	ageAtRead time.Duration
	readAt    time.Time
	Keyframe  bool
	Packets   []Packet
}

// ReplayAge uses one clock for Redis append/read, independent of host wall clocks.
// Memory frames retain local receipt-time semantics.
func (f Frame) ReplayAge(now time.Time) time.Duration {
	if !f.readAt.IsZero() {
		return f.ageAtRead + now.Sub(f.readAt)
	}
	return now.Sub(f.Arrival)
}

// Store can be implemented by a remote cache. Implementations copy on append
// and read. Callers append only complete frames, in source packet order.
type Store interface {
	Append(context.Context, string, Frame) error
	Current(context.Context, string, Track) ([]Frame, error)
	DeleteSession(context.Context, string) error
}

// Limits bounds retained payload per track and, for Memory, across sessions.
// Redis uses Bytes, Frames and IdleTTL; TotalBytes and Sessions are Memory-only.
// Zero values select 8 MiB/300 frames per track, 64 MiB/1024 sessions globally,
// and a 30 s idle TTL. Global expiry runs lazily at most once a second;
// append and read also expire their requested session before touching it.
type Limits struct {
	Bytes      int
	Frames     int
	TotalBytes int
	Sessions   int
	IdleTTL    time.Duration
}

var ErrInvalidFrame = errors.New("framecache: incomplete or invalid frame")

type group struct {
	frames []Frame
	bytes  int
}
type groups struct{ current group }
type cachedSession struct {
	id      string
	tracks  map[Track]*groups
	bytes   int
	touched time.Time
	element *list.Element
}

// Memory is safe for concurrent use. Groups are never partially evicted.
// An overflowing current group is unavailable until the next keyframe.
type Memory struct {
	mu         sync.Mutex
	limits     Limits
	sessions   map[string]*cachedSession
	lru        list.List
	bytes      int
	nextExpiry time.Time
}

func defaultLimits(limits Limits) Limits {
	if limits.Bytes <= 0 {
		limits.Bytes = 8 << 20
	}
	if limits.Frames <= 0 {
		limits.Frames = 300
	}
	if limits.TotalBytes <= 0 {
		limits.TotalBytes = 64 << 20
	}
	if limits.Sessions <= 0 {
		limits.Sessions = 1024
	}
	if limits.IdleTTL <= 0 {
		limits.IdleTTL = 30 * time.Second
	}
	return limits
}

func NewMemory(limits Limits) *Memory {
	return &Memory{limits: defaultLimits(limits), sessions: make(map[string]*cachedSession)}
}

func (m *Memory) Append(ctx context.Context, id string, frame Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	size, err := frameSize(frame)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	m.expire(now)
	defer m.enforceGlobal()
	session := m.touch(id, now)
	if session == nil {
		session = &cachedSession{id: id, tracks: make(map[Track]*groups), touched: now}
		session.element = m.lru.PushBack(session)
		m.sessions[id] = session
	}
	g := session.tracks[frame.Track]
	if g == nil {
		g = &groups{}
		session.tracks[frame.Track] = g
	}
	oldBytes := g.current.bytes
	defer func() {
		delta := g.current.bytes - oldBytes
		session.bytes += delta
		m.bytes += delta
	}()
	if frame.Keyframe {
		g.current = group{}
	} else {
		if len(g.current.frames) == 0 {
			return nil
		}
		last := g.current.frames[len(g.current.frames)-1]
		if frame.Packets[0].SequenceNumber != last.Packets[len(last.Packets)-1].SequenceNumber+1 || int32(frame.Timestamp-last.Timestamp) <= 0 {
			g.current = group{} // references lost: wait for a complete keyframe
			return nil
		}
	}
	if g.current.bytes+size > m.limits.Bytes || len(g.current.frames)+1 > m.limits.Frames {
		g.current = group{}
		return nil
	}
	g.current.frames = append(g.current.frames, cloneFrame(frame))
	g.current.bytes += size
	return nil
}

func (m *Memory) Current(ctx context.Context, id string, track Track) ([]Frame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.expire(now)
	session := m.touch(id, now)
	if session == nil {
		return nil, nil
	}
	if g := session.tracks[track]; g != nil {
		frames := make([]Frame, len(g.current.frames))
		for i, f := range g.current.frames {
			frames[i] = cloneFrame(f)
		}
		return frames, nil
	}
	return nil, nil
}

func (m *Memory) DeleteSession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[id]; session != nil {
		m.remove(session)
	}
	return nil
}

func cloneFrame(f Frame) Frame {
	packets := make([]Packet, len(f.Packets))
	for i, p := range f.Packets {
		packets[i] = p
		packets[i].Payload = append([]byte(nil), p.Payload...)
	}
	f.Packets = packets
	return f
}

// Called under mu. The LRU makes eviction independent of the session count.
// A session's tracks always leave together, including an oversized group.
func (m *Memory) remove(session *cachedSession) {
	m.bytes -= session.bytes
	delete(m.sessions, session.id)
	m.lru.Remove(session.element)
}
func (m *Memory) touch(id string, now time.Time) *cachedSession {
	session := m.sessions[id]
	if session == nil {
		return nil
	}
	if now.Sub(session.touched) >= m.limits.IdleTTL {
		m.remove(session)
		return nil
	}
	session.touched = now
	m.lru.MoveToBack(session.element)
	return session
}
func (m *Memory) expire(now time.Time) {
	if now.Before(m.nextExpiry) {
		return
	}
	m.nextExpiry = now.Add(time.Second)
	for oldest := m.lru.Front(); oldest != nil; oldest = m.lru.Front() {
		session := oldest.Value.(*cachedSession)
		if now.Sub(session.touched) < m.limits.IdleTTL {
			break
		}
		m.remove(session)
	}
}
func (m *Memory) enforceGlobal() {
	for m.bytes > m.limits.TotalBytes || len(m.sessions) > m.limits.Sessions {
		m.remove(m.lru.Front().Value.(*cachedSession))
	}
}
