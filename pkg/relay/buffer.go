package relay

import (
	"container/list"
	"encoding/binary"
	"net/netip"
	"time"

	"github.com/pion/rtp"
)

const (
	DefaultBufferWindow          = time.Second
	DefaultMaxBufferedBytes      = 1 << 20
	DefaultMaxTotalBufferedBytes = 64 << 20
	DefaultMaxBufferedSSRCs      = 16
)

// BufferConfig enables the caller SRTP cache. A nil Config.Buffer retains
// the phase-1 cache/PLI path; a non-nil value selects these bounded defaults.
// The byte budget includes relay-header room. SSRC metadata is bounded too.
type BufferConfig struct {
	Window          time.Duration
	MaxSessionBytes int
	MaxTotalBytes   int
	MaxSSRCs        int
	// Replay sends at most 16 packets or 16 KiB each millisecond by default.
	ReplayInterval     time.Duration
	ReplayBatchPackets int
	ReplayBatchBytes   int
}

func (c BufferConfig) defaults() BufferConfig {
	if c.Window <= 0 {
		c.Window = DefaultBufferWindow
	}
	if c.MaxSessionBytes <= 0 {
		c.MaxSessionBytes = DefaultMaxBufferedBytes
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = DefaultMaxTotalBufferedBytes
	}
	if c.MaxSSRCs <= 0 {
		c.MaxSSRCs = DefaultMaxBufferedSSRCs
	}
	if c.ReplayInterval <= 0 {
		c.ReplayInterval = time.Millisecond
	}
	if c.ReplayBatchPackets <= 0 {
		c.ReplayBatchPackets = 16
	}
	if c.ReplayBatchBytes <= 0 {
		c.ReplayBatchBytes = 16 << 10
	}
	return c
}

type bufferedPacket struct {
	heldPacket
	session       string
	at            time.Time
	ssrc          uint32
	index         uint64
	global, local *list.Element
}

type bufferedSession struct {
	packets *list.List
	bytes   int
	// Highest indexes survive time expiry, so silent tracks retain their ROC.
	indexes   map[uint32]uint64
	trimmed   map[uint32]uint64
	untracked bool
}

// packetBuffer is protected by Relay.forwardMu. Its two linked lists give
// constant-time oldest eviction per session and across the whole relay.
type packetBuffer struct {
	cfg                       BufferConfig
	sessions                  map[string]*bufferedSession
	packets                   *list.List
	bytes                     int
	maxSessions               int
	drops, expired, untracked uint64
}

func newPacketBuffer(cfg BufferConfig) *packetBuffer {
	return &packetBuffer{cfg: cfg.defaults(), sessions: make(map[string]*bufferedSession), packets: list.New(), maxSessions: DefaultMaxFlows}
}

// callerIndex reads only the clear RTP header. RTP/RTCP mux reserves payload
// types 64-95; RTCP (192-223, including its marker bit) is excluded. Historic
// DTLS, STUN and SRTCP are not replayable from an inbound SRTP checkpoint.
func callerIndex(packet []byte) (ssrc uint32, seq uint16, ok bool) {
	if len(packet) < 12 || packet[0]>>6 != 2 || packet[1] >= 192 && packet[1] <= 223 {
		return 0, 0, false
	}
	var header rtp.Header
	if _, err := header.Unmarshal(packet); err != nil {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(packet[8:12]), binary.BigEndian.Uint16(packet[2:4]), true
}

// inboundIndex is RFC 3711's half-space ROC estimate, in the CALLER's
// inbound space. It must never be compared with a worker outbound replay floor.
func inboundIndex(highest uint64, seq uint16) uint64 {
	roc, last := highest>>16, uint16(highest) //nolint:gosec // low 16 bits of an SRTP index
	if last < 1<<15 {
		if seq > last && seq-last > 1<<15 && roc > 0 {
			roc--
		}
	} else if last-(1<<15) > seq {
		roc++
	}
	return roc<<16 | uint64(seq)
}

func (b *packetBuffer) add(id string, caller netip.AddrPort, packet []byte, now time.Time) {
	ssrc, seq, ok := callerIndex(packet)
	if !ok {
		return
	}
	b.expire(now)
	s := b.sessions[id]
	if s == nil {
		if len(b.sessions) >= b.maxSessions {
			b.untracked++
			return
		}
		s = &bufferedSession{packets: list.New(), indexes: make(map[uint32]uint64), trimmed: make(map[uint32]uint64)}
		b.sessions[id] = s
	}
	highest, seen := s.indexes[ssrc]
	if !seen && len(s.indexes) >= b.cfg.MaxSSRCs {
		s.untracked = true
		b.untracked++
		return
	}
	index := uint64(seq)
	if seen {
		index = inboundIndex(highest, seq)
	}
	if !seen || index > highest {
		s.indexes[ssrc] = index
	}
	size := MaxHeaderLen + len(packet)
	// Evict oldest first even when one incoming datagram cannot fit.
	for s.bytes+size > b.cfg.MaxSessionBytes && s.packets.Len() > 0 {
		b.remove(s.packets.Front().Value.(*bufferedPacket), true)
	}
	for b.bytes+size > b.cfg.MaxTotalBytes && b.packets.Len() > 0 {
		b.remove(b.packets.Front().Value.(*bufferedPacket), true)
	}
	if size > b.cfg.MaxSessionBytes || size > b.cfg.MaxTotalBytes {
		s.trimmed[ssrc] = max(s.trimmed[ssrc], index)
		b.drops++
		return
	}
	data := make([]byte, size)
	copy(data[MaxHeaderLen:], packet)
	p := &bufferedPacket{heldPacket: heldPacket{caller: caller, datagram: data}, session: id, at: now, ssrc: ssrc, index: index}
	p.global = b.packets.PushBack(p)
	p.local = s.packets.PushBack(p)
	s.bytes += size
	b.bytes += size
}

func (b *packetBuffer) remove(p *bufferedPacket, overflow bool) {
	s := b.sessions[p.session]
	s.packets.Remove(p.local)
	b.packets.Remove(p.global)
	s.bytes -= len(p.datagram)
	b.bytes -= len(p.datagram)
	s.trimmed[p.ssrc] = max(s.trimmed[p.ssrc], p.index)
	if overflow {
		b.drops++
	}
}

func (b *packetBuffer) expire(now time.Time) {
	for p := b.packets.Front(); p != nil; p = b.packets.Front() {
		packet := p.Value.(*bufferedPacket)
		if now.Sub(packet.at) < b.cfg.Window {
			break
		}
		b.remove(packet, false)
		b.expired++
	}
}

func (b *packetBuffer) forget(id string) {
	if s := b.sessions[id]; s != nil {
		for p := s.packets.Front(); p != nil; p = s.packets.Front() {
			b.remove(p.Value.(*bufferedPacket), false)
		}
		delete(b.sessions, id)
	}
}
