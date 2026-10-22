package mediaworker

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/pion/rtp"
	"github.com/relaishq/relais/internal/opuspacket"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

const (
	playbackWindowDuration = 300 * time.Millisecond
	playbackWindowBytes    = 4 << 10
	playbackRebaseAfter    = 20 * time.Millisecond
	// RTP header plus the largest negotiated SRTP tag must fit receiveMTU.
	opusPacketMTU = receiveMTU - 12 - 16
)

type hostTimer struct {
	ID         string    `json:"i"`
	Generation uint64    `json:"g"`
	At         time.Time `json:"a,omitzero"`
}

const timerIDMaxBytes = 128

// Called only by the actor. Each due generation is its own bounded transaction.
func (s *session) deliverClockEvent() error {
	now := s.taskNow()
	for _, timer := range s.agent.latest.V2.Timers {
		if !timer.At.IsZero() && !now.Before(timer.At) {
			id := clockEventID("timer", timer.ID, timer.Generation)
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.TimerFired, ID: id, Timer: &agent.TimerNotice{ID: timer.ID, Generation: timer.Generation}}, eventLive)
			return err
		}
	}
	s.mu.Lock()
	notify := slices.Clone(s.agent.v2.playback.notify)
	queue := s.agent.v2.playback.queue
	var finished *agent.Turn
	for _, turn := range notify {
		busy := slices.ContainsFunc(queue, func(a agent.Audio) bool { return a.Turn == turn })
		for _, task := range s.agent.v2.tasks {
			if task.intent.Turn == turn && (!task.intent.Final || len(task.results) > 0) {
				busy = true
			}
		}
		for _, refusal := range s.agent.latest.V2.Capacity {
			if refusal.Turn == turn {
				busy = true
			}
		}
		if !busy {
			t := turn
			finished = &t
			break
		}
	}
	s.mu.Unlock()
	if finished != nil {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.PlaybackFinished, ID: clockEventID("playback", "", uint64(*finished)), Playback: &agent.PlaybackNotice{Turn: *finished}}, eventLive)
		return err
	}
	return nil
}

// Versioned, length-prefixed event identity; IDs may contain any UTF-8 text.
func clockEventID(kind, id string, generation uint64) agent.EventID {
	return agent.EventID("relais/clock/v1/" + strconv.Itoa(len(kind)) + ":" + kind + strconv.Itoa(len(id)) + ":" + id + "/" + strconv.FormatUint(generation, 10))
}

type audioKey struct {
	task     agent.TaskID
	sequence uint64
}
type playbackPayload struct {
	audio     *agent.EncodedAudio
	durations []time.Duration
}

// Consecutive offsets in the same chunk share one compact crash reference.
type audioSpan struct {
	Task     agent.TaskID `json:"t"`
	Sequence uint64       `json:"s"`
	Turn     agent.Turn   `json:"u,omitempty"`
	Offset   uint32       `json:"o,omitempty"`
	Count    uint32       `json:"n"`
}
type playbackState struct {
	NextAt         time.Time     `json:"a,omitzero"`
	NextTS         uint32        `json:"t,omitempty"`
	Initialized    bool          `json:"i,omitempty"`
	Pending        int           `json:"p,omitempty"`
	Length         int           `json:"l,omitempty"`
	Duration       time.Duration `json:"d,omitempty"`
	referenceBytes int
	Window         []audioSpan  `json:"w,omitempty"`
	Notify         []agent.Turn `json:"f,omitempty"`
}
type playbackRuntime struct {
	queue          []agent.Audio
	payloads       map[audioKey]playbackPayload
	nextAt         time.Time
	nextTS         uint32
	initialized    bool
	length         int
	duration       time.Duration
	referenceBytes int
	notify         []agent.Turn
}

func audioRefsBytes(refs []agent.Audio) int {
	n := 0
	for _, ref := range refs {
		n += len(ref.Source.TaskID) + 32
	}
	return n
}
func playbackEqual(a, b playbackState) bool {
	return a.NextAt.Equal(b.NextAt) && a.NextTS == b.NextTS && a.Initialized == b.Initialized && a.Pending == b.Pending && a.Length == b.Length && a.Duration == b.Duration && slices.Equal(a.Window, b.Window) && slices.Equal(a.Notify, b.Notify)
}
func (p *playbackRuntime) packet(a agent.Audio) ([]byte, time.Duration, bool) {
	payload, ok := p.payloads[audioKey{a.Source.TaskID, a.Source.Sequence}]
	offset := int(a.Source.PacketOffset)
	if !ok || offset >= len(payload.audio.Packets) {
		return nil, 0, false
	}
	return payload.audio.Packets[offset], payload.durations[offset], true
}
func (p playbackRuntime) snapshot() playbackState {
	state := playbackState{NextAt: p.nextAt, NextTS: p.nextTS, Initialized: p.initialized, Pending: len(p.queue), Length: p.length, Duration: p.duration, referenceBytes: p.referenceBytes, Notify: slices.Clone(p.notify)}
	var duration time.Duration
	bytes := 0
	for _, ref := range p.queue {
		payload, d, ok := p.packet(ref)
		if !ok {
			break
		}
		// Only the bounded reference prefix is crash-persistent; bytes stay move-only.
		if duration+d > playbackWindowDuration || bytes+len(payload) > playbackWindowBytes {
			break
		}
		duration += d
		bytes += len(payload)
		n := len(state.Window)
		if n > 0 && state.Window[n-1].Task == ref.Source.TaskID && state.Window[n-1].Sequence == ref.Source.Sequence && state.Window[n-1].Turn == ref.Turn && state.Window[n-1].Offset+state.Window[n-1].Count == ref.Source.PacketOffset {
			state.Window[n-1].Count++
		} else if n == 0 {
			state.Window = append(state.Window, audioSpan{Task: ref.Source.TaskID, Sequence: ref.Source.Sequence, Turn: ref.Turn, Offset: ref.Source.PacketOffset, Count: 1})
		} else {
			break
		}
	}
	return state
}
func (s *session) playbackSnapshotLocked() playbackState { return s.agent.v2.playback.snapshot() }

func (s *session) wakePacer() {
	select {
	case s.agent.v2.wakePlayback <- struct{}{}:
	default:
	}
}

func (s *session) registerAudioLocked(r agent.TaskResult) error {
	if r.Chunk.Audio == nil {
		return nil
	}
	durations, err := validateOpus(r.Chunk.Audio)
	if err != nil {
		return err
	}
	p := &s.agent.v2.playback
	if p.payloads == nil {
		p.payloads = make(map[audioKey]playbackPayload)
	}
	key := audioKey{r.TaskID, r.Sequence}
	if _, exists := p.payloads[key]; exists {
		return fmt.Errorf("%w: duplicate audio payload", agent.ErrIncompatibleAgent)
	}
	p.payloads[key] = playbackPayload{r.Chunk.Audio, durations}
	return nil
}
func (s *session) restorePlayback(move *movePayload) error {
	p := &s.agent.v2.playback
	saved := s.agent.latest.V2.Playback
	p.nextAt, p.nextTS, p.initialized = saved.NextAt, saved.NextTS, saved.Initialized
	p.notify = slices.Clone(saved.Notify)
	for _, t := range s.agent.v2.tasks {
		for _, rs := range [][]agent.TaskResult{t.audio, t.results} {
			for _, r := range rs {
				if err := s.registerAudioLocked(r); err != nil {
					return err
				}
			}
		}
	}
	// Payloads retained for deduplication count too, even after playout. A
	// different worker's local ceiling must never silently discard accepted audio.
	bounds := s.agent.v2.registration.Bounds
	retainedBytes := 0
	var retainedDuration time.Duration
	for _, payload := range p.payloads {
		for i, packet := range payload.audio.Packets {
			retainedBytes += len(packet)
			retainedDuration += payload.durations[i]
		}
	}
	if retainedBytes > bounds.MaxAudioBytes || (bounds.MaxAudioDuration > 0 && retainedDuration > bounds.MaxAudioDuration) {
		return fmt.Errorf("%w: restored audio payload exceeds local limit", agent.ErrCapacity)
	}
	if move != nil {
		p.queue = slices.Clone(move.Audio)
		p.length = 0
		p.duration = 0
		p.referenceBytes = 0
		for _, ref := range p.queue {
			task := slices.IndexFunc(s.agent.v2.tasks, func(t *localTask) bool { return t.intent.ID == ref.Source.TaskID })
			if task < 0 || s.agent.v2.tasks[task].intent.Turn != ref.Turn || ref.Source.Sequence > s.agent.v2.tasks[task].intent.Cursor {
				return fmt.Errorf("%w: invalid exact audio reference", agent.ErrIncompatibleAgent)
			}
			if data, d, ok := p.packet(ref); !ok {
				return fmt.Errorf("%w: missing exact audio payload", agent.ErrIncompatibleAgent)
			} else {
				p.length += len(data)
				p.duration += d
				p.referenceBytes += len(ref.Source.TaskID) + 32
			}
		}
		actual := p.snapshot()
		// Read initial experimental v8 exports whose window had multiple spans
		// and no cached duration. Exact payload/reference validation still applies.
		if saved.Duration == 0 {
			saved.Duration = actual.Duration
			if len(saved.Window) > 1 {
				saved.Window = saved.Window[:1]
			}
		}
		if !playbackEqual(actual, saved) {
			return fmt.Errorf("%w: exact playback window mismatch", agent.ErrIncompatibleAgent)
		}
	} else if saved.Pending > 0 {
		// No persisted reference is activated without its payload. The notice tells
		// the application to clear unsent speech and restart on a later event.
		p.notify = nil
	}
	if p.initialized {
		p.rebase(s.taskNow())
	}
	return nil
}

// rtpClockTicks avoids overflowing an int64 nanosecond duration when a saved
// clock is far in the past. RTP timestamp wrap is intentional.
func rtpClockTicks(elapsed time.Duration) uint32 {
	return uint32(uint64(elapsed/time.Second)*48000 + uint64(elapsed%time.Second)*48000/uint64(time.Second))
}
func (p *playbackRuntime) rebase(now time.Time) {
	if !p.nextAt.IsZero() && now.After(p.nextAt) {
		p.nextTS += rtpClockTicks(now.Sub(p.nextAt))
		p.nextAt = now
	}
}

// A dedicated output loop keeps DTX and slow callbacks out of the send clock.
// The session lock is the ADR 0009 export fence, shared with encryption/cursors.
func (s *session) playbackLoop() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		delay, err := s.paceOne()
		if err != nil {
			s.agentFailed(err)
			return
		}
		timer.Reset(delay)
		select {
		case <-s.ctx.Done():
			return
		case <-s.agent.v2.wakePlayback:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}
func (s *session) paceOne() (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &s.agent.v2.playback
	if s.agent.paused || s.worker.paused.Load() || s.fenced.Load() || s.ctx.Err() != nil || s.srtpOut == nil || len(p.queue) == 0 {
		return time.Hour, nil
	}
	now := s.taskClockLocked()
	if now.Before(p.nextAt) {
		return p.nextAt.Sub(now), nil
	}
	ref := p.queue[0]
	payload, duration, ok := p.packet(ref)
	if !ok {
		return 0, fmt.Errorf("%w: missing paced audio payload", agent.ErrIncompatibleAgent)
	}
	clockAt, clockTS, clockInitialized := p.nextAt, p.nextTS, p.initialized
	if !p.initialized {
		p.nextTS = s.state.Audio.InitialTS
		p.initialized = true
		p.nextAt = now
	}
	// Small scheduler delays are absorbed by the absolute schedule. A stall
	// beyond 20 ms rebases once, including RTP media time.
	if now.Sub(p.nextAt) >= playbackRebaseAfter {
		p.rebase(now)
	}
	header := rtp.Header{Version: 2, PayloadType: s.state.Audio.PayloadType, SSRC: s.state.Audio.SSRC, Timestamp: p.nextTS}
	if err := s.sendAgentAudioLocked(header, payload, s.state.Agent); err != nil {
		p.nextAt, p.nextTS, p.initialized = clockAt, clockTS, clockInitialized
		return 0, fmt.Errorf("paced audio encryption: %w", err)
	}
	p.nextAt = p.nextAt.Add(duration)
	p.nextTS += rtpClockTicks(duration)
	p.length -= len(payload)
	p.duration -= duration
	p.referenceBytes -= len(ref.Source.TaskID) + 32
	p.queue[0] = agent.Audio{}
	p.queue = p.queue[1:]
	s.state.Agent.V2.Playback = p.snapshot()
	return max(0, p.nextAt.Sub(s.taskClockLocked())), nil
}

// RFC 6716 section 3 framing validation (https://www.rfc-editor.org/rfc/rfc6716.html#section-3).
// Only encoded packet framing is parsed; decoding Opus is outside the host.
func validateOpus(audio *agent.EncodedAudio) ([]time.Duration, error) {
	if audio.Format != "audio/opus" {
		return nil, fmt.Errorf("%w: unsupported encoded audio format", agent.ErrIncompatibleAgent)
	}
	durations := make([]time.Duration, len(audio.Packets))
	for i, p := range audio.Packets {
		d, err := opusDuration(p)
		if err != nil {
			return nil, err
		}
		durations[i] = d
	}
	return durations, nil
}
func opusDuration(p []byte) (time.Duration, error) {
	if len(p) > opusPacketMTU {
		return 0, fmt.Errorf("%w: invalid Opus packet", agent.ErrIncompatibleAgent)
	}
	d, err := opuspacket.Duration(p)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", agent.ErrIncompatibleAgent, err)
	}
	return d, nil
}

// Audio limits count actual immutable payloads, including chunks which have not
// reached the callback yet and consumed chunks retained for replay/reference use.
func (s *session) admitTaskAudio(audio *agent.EncodedAudio) error {
	durations, err := validateOpus(audio)
	if err != nil {
		return err
	}
	bounds := s.agent.v2.registration.Bounds
	bytes := 0
	var duration time.Duration
	for i, packet := range audio.Packets {
		bytes += len(packet)
		duration += durations[i]
	}
	if (bounds.MaxAudioBytes > 0 && bytes > bounds.MaxAudioBytes) || (bounds.MaxAudioDuration > 0 && duration > bounds.MaxAudioDuration) {
		return agent.ErrCapacity
	}
	s.mu.Lock()
	for _, payload := range s.agent.v2.playback.payloads {
		for i, packet := range payload.audio.Packets {
			bytes += len(packet)
			duration += payload.durations[i]
		}
	}
	s.mu.Unlock()
	if (bounds.MaxAudioBytes > 0 && bytes > bounds.MaxAudioBytes) || (bounds.MaxAudioDuration > 0 && duration > bounds.MaxAudioDuration) {
		return agent.ErrBackpressure
	}
	return nil
}
