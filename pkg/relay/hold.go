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

func (r *Relay) acknowledgeBarrier(id uint64, from netip.AddrPort) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()

	// Holds are bounded; acknowledgements never allocate lookup state.
	for _, h := range r.holds {
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
		h.replayComplete = false
		h.replayResult.Expired = true
		h.replayResult.Dropped += len(h.queue)
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
	delete(r.holds, h.id)
	h.timer.Stop()
	close(h.released)
	worker := to
	if !worker.IsValid() {
		worker = r.flows.sessionWorker(h.id, h.worker)
	}

	count := len(h.queue)
	if h.replay {
		orderReplay(h.queue)
	}
	for _, packet := range h.queue {
		if floor, known := h.delivered[packet.ssrc]; h.replay && packet.media && known && packet.index <= floor {
			r.replayFiltered.Add(1)
			continue
		}
		if h.replay && packet.media {
			h.delivered[packet.ssrc] = packet.index
		}
		if !r.sendHeld(h.id, packet, worker) {
			r.holdSendFailures.Add(1)
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
