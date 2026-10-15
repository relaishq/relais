package relay

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/relais/internal/relayleg"
)

var (
	// ErrBarrierTimeout means the source did not drain promptly. No export
	// may follow; held packets have already been released to the current route.
	ErrBarrierTimeout = errors.New("relay: drain barrier acknowledgement timed out")
	// ErrHeld means a session already has a planned hold.
	ErrHeld = errors.New("relay: session already held")
	// ErrHoldLimit means the bounded set of concurrent holds is full.
	ErrHoldLimit = errors.New("relay: concurrent hold limit reached")
	// ErrHoldExpired means a hold auto-released before coordination finished.
	ErrHoldExpired = errors.New("relay: hold expired")
)

// heldPacket owns a copy of one confirmed caller's queued datagram.
type heldPacket struct {
	caller   netip.AddrPort
	datagram []byte // includes room for the relay header
	ssrc     uint32
	index    uint64
	media    bool
}

// sessionHold brackets one move with a drain barrier and a bounded queue.
// Its fields are protected by forwardMu; timeout releases it automatically.
type sessionHold struct {
	id               string
	barrier          uint64
	source           netip.AddrPort
	worker           netip.AddrPort
	ready            chan struct{}
	acknowledged     bool
	barrierTimedOut  bool
	released         chan struct{}
	timer            *time.Timer
	queue            []heldPacket
	bytes            int
	replay           bool
	replaying        bool
	delivered        map[uint32]uint64
	replayInbound    map[uint32]uint64
	replayTrimmed    map[uint32]uint64
	replayComplete   bool
	replayCheckpoint bool
	replayResult     ReplayResult
	queuedHigh       map[uint32]uint64
	replayGeneration uint64
	replayTarget     netip.AddrPort
	replayTenure     uint64
	// drainNonce and drained track the post-export output drain marker.
	drainNonce uint64
	drained    chan struct{}
}

// HoldSession brackets a planned move. Caller packets stop being forwarded
// before a barrier is sent to the old worker. That worker acknowledges from
// its read loop after finishing preceding datagrams; only then may it export.
// BarrierTimeout bounds this pre-export wait independently of ctx. On a
// missing acknowledgement the hold releases to the current route and the
// move aborts. HoldTimeout starts only after acknowledgement, bounding the
// later coordination phase. Queue memory and concurrent holds are bounded.
func (r *Relay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	from = unmap(from)
	if id == "" || !r.registered(from) {
		return errors.New("relay: hold needs a session and registered source")
	}

	r.forwardMu.Lock()
	if r.ctx.Err() != nil {
		r.forwardMu.Unlock()
		return errors.New("relay: closed")
	}
	if r.holds[id] != nil {
		r.forwardMu.Unlock()
		return ErrHeld
	}
	if len(r.holds) >= r.cfg.MaxHeldSessions {
		r.forwardMu.Unlock()
		return ErrHoldLimit
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		r.forwardMu.Unlock()
		return err
	}
	h := &sessionHold{id: id, barrier: binary.BigEndian.Uint64(nonce[:]), source: from, worker: from, ready: make(chan struct{}), released: make(chan struct{})}
	r.holds[id] = h
	h.timer = time.AfterFunc(r.cfg.BarrierTimeout, func() { r.expireBarrier(h) })
	_, err := r.workers.WriteToUDPAddrPort(relayleg.Barrier(h.barrier, false), from)
	r.forwardMu.Unlock()
	if err != nil {
		_, _ = r.ReleaseSession(id, from)
		return err
	}
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.forwardMu.Lock()
			// Resend the same nonce only in the drain phase. Duplicate ACKs are
			// harmless; the original deadline is never extended.
			if r.holds[id] == h && !h.acknowledged {
				r.barrierResends.Add(1)
				_, _ = r.workers.WriteToUDPAddrPort(relayleg.Barrier(h.barrier, false), from)
			}
			r.forwardMu.Unlock()
		case <-h.ready:
			r.forwardMu.Lock()
			active := r.holds[id] == h
			r.forwardMu.Unlock()
			if !active {
				return ErrHoldExpired
			}
			return nil
		case <-h.released:
			if h.barrierTimedOut {
				return ErrBarrierTimeout
			}
			return ErrHoldExpired
		case <-r.ctx.Done():
			return errors.New("relay: closed")
		case <-ctx.Done():
			_, _ = r.ReleaseSession(id, netip.AddrPort{})
			return ctx.Err()
		}
	}
}

// ReleaseSession ends a hold after the selected owner has resumed. Held
// caller packets (including consent) are sent before any newly arriving
// packet, in order. to is the resumed worker; a zero address uses the current
// confirmed route. Rollback supplies the source after resuming it.
func (r *Relay) ReleaseSession(id string, to netip.AddrPort) (int, error) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()

	h := r.holds[id]
	if h == nil {
		return 0, ErrHoldExpired
	}

	return r.releaseHold(h, to), nil
}

// ConsumedInput is one caller SSRC of a final planned export (ADR 0009):
// the highest authenticated inbound index and, when Exact, the 64-index
// receive window below it (bit i: High-i was consumed).
type ConsumedInput struct {
	High   uint64 `json:"high"`
	Window uint64 `json:"window,omitempty"`
	Exact  bool   `json:"exact,omitempty"`
}

// exportWindow matches the workers' SRTP replay window.
const exportWindow = 64

// keep reports whether media at index is unconsumed by the export. Held
// input that an inexact export cannot classify keeps today's behavior.
func (in ConsumedInput) keep(index uint64, held bool) bool {
	switch {
	case index > in.High:
		return true
	case !in.Exact:
		return held
	case in.High-index >= exportWindow:
		return false // SRTP rejects it as too old, with or without a move
	default:
		return in.Window&(1<<(in.High-index)) == 0
	}
}

// ReleaseAfterExport ends a planned hold after `to` resumed from the exact,
// margin-zero export that input describes. Besides the held queue, it sends
// retained caller media the export did not consume: a datagram reordered
// behind the drain marker reached the source after its fence and was
// discarded there. Held media the export already consumed is not sent.
// Selected media is ordered per SSRC and sent before the hold is removed,
// so live input cannot overtake it. Without a caller buffer only the held
// queue can be sent. A crash replay gate is released as by ReleaseSession.
func (r *Relay) ReleaseAfterExport(id string, to netip.AddrPort, input map[uint32]ConsumedInput) (ReleaseReply, error) {
	for _, in := range input {
		if in.High >= 1<<48 {
			return ReleaseReply{}, errors.New("relay: inbound SRTP index exceeds 48 bits")
		}
	}
	to = unmap(to)
	r.forwardMu.Lock()
	locked := time.Now()
	defer func() {
		held := time.Since(locked)
		r.forwardMu.Unlock()
		r.plannedLockNanos.Add(uint64(held)) //nolint:gosec // non-negative
		for {
			longest := r.plannedLockMax.Load()
			if uint64(held) <= longest || r.plannedLockMax.CompareAndSwap(longest, uint64(held)) { //nolint:gosec // non-negative
				break
			}
		}
	}()
	h := r.holds[id]
	if h == nil {
		return ReleaseReply{}, ErrHoldExpired
	}
	if h.replay {
		return ReleaseReply{Packets: r.releaseHold(h, to)}, nil
	}
	reply := r.releaseExported(h, to, input)
	reply.LockHeld = time.Since(locked)
	return reply, nil
}

// evicted reports whether eviction of the ring's indexes lowest..trimmed
// could have removed input this export did not consume. An unconsumed index
// can also be plain caller loss; the relay cannot tell them apart, so this is
// conservative. Indexes below the ring's first observation are never counted.
func (in ConsumedInput) evicted(lowest, trimmed uint64) bool {
	if trimmed > in.High {
		return true
	}
	if !in.Exact {
		return false
	}
	for distance := in.High - trimmed; distance < exportWindow && distance <= in.High && in.High-distance >= lowest; distance++ {
		if in.Window&(1<<distance) == 0 {
			return true
		}
	}
	return false
}

type mediaKey struct {
	ssrc  uint32
	index uint64
}

// releaseExported runs under forwardMu.
func (r *Relay) releaseExported(h *sessionHold, to netip.AddrPort, input map[uint32]ConsumedInput) ReleaseReply {
	reply := ReleaseReply{Packets: len(h.queue), Complete: true}
	s := r.bufferSession(h.id)
	if s != nil {
		r.buffer.expire(time.Now())
		for ssrc, in := range input {
			r.buffer.align(s, ssrc, in.High)
		}
	} else {
		reply.Complete = false
	}
	// Classify held media in the export's index space. An SSRC the export
	// does not name uses the ring's own space, or stays unclassified.
	sent := make(map[mediaKey]bool, len(h.queue))
	list := make([]heldPacket, 0, len(h.queue))
	for _, p := range h.queue {
		if ssrc, seq, ok := callerIndex(p.datagram[MaxHeaderLen:]); ok {
			if in, named := input[ssrc]; named {
				p.media, p.ssrc, p.index = true, ssrc, inboundIndex(in.High, seq)
				if !in.keep(p.index, true) || sent[mediaKey{ssrc, p.index}] {
					reply.Filtered++
					continue
				}
			} else if highest, tracked := r.trackedIndex(s, ssrc); tracked {
				p.media, p.ssrc, p.index = true, ssrc, inboundIndex(highest, seq)
				if sent[mediaKey{ssrc, p.index}] {
					reply.Filtered++
					continue
				}
			}
			if p.media {
				sent[mediaKey{p.ssrc, p.index}] = true
			}
		}
		list = append(list, p)
	}
	// Retained media the export did not consume precedes held input.
	var recovered []heldPacket
	if s != nil {
		for item := s.packets.Front(); item != nil; item = item.Next() {
			p := item.Value.(*bufferedPacket)
			key := mediaKey{p.ssrc, p.index}
			if sent[key] {
				continue
			}
			if in, named := input[p.ssrc]; named && !in.keep(p.index, false) {
				continue
			}
			sent[key] = true
			recovered = append(recovered, heldPacket{caller: p.caller, datagram: p.datagram, ssrc: p.ssrc, index: p.index, media: true})
		}
		// The ring may have evicted unconsumed input, or never tracked a stream.
		for ssrc, trimmed := range s.trimmed {
			if in, named := input[ssrc]; !named || in.evicted(s.lowest[ssrc], trimmed) {
				reply.Complete = false
			}
		}
		if s.untracked {
			reply.Complete = false
		}
	}
	reply.Recovered = len(recovered)
	list = append(recovered, list...)
	orderReplay(list)
	r.releaseHoldWith(h, to, list)
	r.plannedReleases.Add(1)
	r.plannedRecovered.Add(uint64(reply.Recovered)) //nolint:gosec // non-negative
	r.plannedFiltered.Add(uint64(reply.Filtered))   //nolint:gosec // non-negative
	if !reply.Complete {
		r.plannedIncomplete.Add(1)
	}
	return reply
}

func (r *Relay) trackedIndex(s *bufferedSession, ssrc uint32) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	highest, tracked := s.indexes[ssrc]
	return highest, tracked
}

func (r *Relay) acknowledgeBarrier(id uint64, from netip.AddrPort) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()

	// Holds are bounded; acknowledgements never allocate lookup state.
	for _, h := range r.holds {
		if h.drained != nil && h.drainNonce == id && h.source == from {
			select {
			case <-h.drained:
			default:
				close(h.drained)
			}
			return
		}
		if h.barrier == id && h.source == from && !h.acknowledged {
			h.acknowledged = true
			h.timer.Stop()
			h.timer = time.AfterFunc(r.cfg.HoldTimeout, func() { r.expireHold(h) })
			close(h.ready)
			return
		}
	}
}

func (r *Relay) enqueue(h *sessionHold, caller netip.AddrPort, packet []byte) {
	if h.replay {
		r.enqueueReplay(h, caller, packet)
		return
	}
	size := MaxHeaderLen + len(packet)
	if len(h.queue) >= r.cfg.MaxHeldPackets || h.bytes+size > r.cfg.MaxHeldBytes || r.heldBytes+size > r.cfg.MaxTotalHeldBytes {
		r.holdDrops.Add(1)
		return
	}

	datagram := make([]byte, size)
	copy(datagram[MaxHeaderLen:], packet)
	h.queue = append(h.queue, heldPacket{caller: caller, datagram: datagram})
	h.bytes += size
	r.heldBytes += size
	r.heldPackets++
}

// expireBarrier cannot export a silent source. The timeout callback and
// acknowledgement serialize under forwardMu; only the winning phase applies.
func (r *Relay) expireBarrier(h *sessionHold) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()

	if r.holds[h.id] != h || h.acknowledged {
		return
	}
	h.barrierTimedOut = true
	r.barrierTimeouts.Add(1)
	r.releaseHold(h, netip.AddrPort{})
}

func (r *Relay) expireHold(h *sessionHold) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()

	if r.holds[h.id] != h {
		return
	}

	r.holdTimeouts.Add(1)
	if h.replay {
		// An abandoned crash replay has no acknowledged live recipient. End
		// the gate without flushing old ciphertext to an unadopted/dead leg.
		r.markReplayExpired(h)
		r.saveReplay(h)
		r.discardHold(h)
	} else {
		r.releaseHold(h, netip.AddrPort{})
	}
}

// discardHold runs under forwardMu. Terminal cleanup never sends its queue.
func (r *Relay) discardHold(h *sessionHold) {
	delete(r.holds, h.id)
	h.timer.Stop()
	close(h.released)
	r.heldBytes -= h.bytes
	r.heldPackets -= len(h.queue)
	h.queue, h.bytes = nil, 0
}

// releaseHold runs under forwardMu and sends without routeMu. Re-admitting
// held binding requests preserves consent authentication for confirmed callers.
func (r *Relay) releaseHold(h *sessionHold, to netip.AddrPort) int {
	if h.replay {
		orderReplay(h.queue)
	}
	return r.releaseHoldWith(h, to, h.queue)
}

// releaseHoldWith removes the hold and sends list in order. Accounting still
// releases every packet the hold queued, including any left out of list.
func (r *Relay) releaseHoldWith(h *sessionHold, to netip.AddrPort, list []heldPacket) int {
	delete(r.holds, h.id)
	h.timer.Stop()
	close(h.released)
	worker := to
	if !worker.IsValid() {
		worker = r.flows.sessionWorker(h.id, h.worker)
	}

	count := len(h.queue)
	for _, packet := range list {
		if floor, known := h.delivered[packet.ssrc]; h.replay && packet.media && known && packet.index <= floor {
			r.replayFiltered.Add(1)
			continue
		}
		if r.sendHeld(h.id, packet, worker) {
			if h.replay {
				r.noteReplayDelivery(h, packet, worker)
			}
		} else {
			r.holdSendFailures.Add(1)
			if h.replay {
				h.replayComplete = false
				h.replayResult.SendFailures++
				r.replaySendFailures.Add(1)
			}
		}
	}
	r.heldBytes -= h.bytes
	r.heldPackets -= count
	return count
}

func (r *Relay) sendHeld(id string, packet heldPacket, worker netip.AddrPort) bool {
	confirmed, ok := r.flows.forwardRoute(packet.caller)
	if !ok || confirmed.session != id {
		return false
	}
	payload := packet.datagram[MaxHeaderLen:]
	if isSTUN(payload) {
		session, tx, request := parseBindingRequest(payload)
		if request && !r.flows.admit(packet.caller, worker, session, tx, time.Now()) {
			return false
		}
		if request && isNomination(payload) {
			r.flows.markNomination(packet.caller, tx)
		}
	}
	return r.sendCaller(packet.datagram, packet.caller, worker)
}
