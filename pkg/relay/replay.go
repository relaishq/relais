package relay

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"sort"
	"time"
)

var (
	ErrBufferDisabled   = errors.New("relay: caller buffer disabled")
	ErrReplayInProgress = errors.New("relay: replay already running")
	ErrReplayNotReady   = errors.New("relay: replay checkpoint not supplied")
)

// ReplayPlan reports whether the retained ring covers the checkpoint. An
// incomplete cache keeps the worker's ordinary frame-cache/PLI fallback.
type ReplayPlan struct {
	Packets  int  `json:"packets"`
	Complete bool `json:"complete"`
}

// ReplayResult includes loss observed after preparation or worker adoption.
// Dropped counts ring-copy/live admission drops and packets discarded on expiry;
// historical ring gaps clear Complete without inventing a missing-packet count.
type ReplayResult struct {
	Packets      int           `json:"packets"`
	Filtered     int           `json:"filtered"`
	Duration     time.Duration `json:"duration"`
	Dropped      int           `json:"dropped"`
	SendFailures int           `json:"send_failures"`
	Expired      bool          `json:"expired"`
	Complete     bool          `json:"complete"`
}

// replayReceipt is retained with bounded session metadata, until flow removal.
type replayReceipt struct {
	source, target netip.AddrPort
	inbound        map[uint32]uint64
	result         ReplayResult
}

func (r *Relay) saveReplay(h *sessionHold) {
	h.replayResult.Complete = h.replayComplete && !h.replayResult.Expired
	if h.replayCheckpoint && r.buffer != nil {
		if current := r.holds[h.id]; current != nil && current != h {
			return
		}
		if s := r.buffer.sessions[h.id]; s != nil && s.replayGeneration == h.replayGeneration {
			s.receipt = &replayReceipt{source: h.source, target: h.worker, inbound: maps.Clone(h.replayInbound), result: h.replayResult}
		}
	}
}

// BeginReplay gates live traffic BEFORE rerouting/resuming. It needs no
// acknowledgement from a dead source. It copies the cache into the bounded
// #6 hold, filtering in the caller's inbound index space, not outbound space.
// Nil inbound installs only the gate, before snapshot I/O. Call again with
// the exact snapshot's non-nil inbound map before resume (empty is valid).
func (r *Relay) BeginReplay(ctx context.Context, id string, from netip.AddrPort, inbound map[uint32]uint64) (ReplayPlan, error) {
	if err := ctx.Err(); err != nil {
		return ReplayPlan{}, err
	}
	from = unmap(from)
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()
	if r.ctx.Err() != nil {
		return ReplayPlan{}, errors.New("relay: closed")
	}
	if r.buffer == nil {
		return ReplayPlan{}, ErrBufferDisabled
	}
	if id == "" || !r.registered(from) {
		return ReplayPlan{}, errors.New("relay: replay needs a session and registered source")
	}
	current := r.flows.sessionWorker(id, from)
	if s := r.buffer.sessions[id]; s != nil && s.receipt != nil {
		receipt := s.receipt
		if r.holds[id] == nil && receipt.source == from && receipt.target == current && (inbound == nil && receipt.source != receipt.target || inbound != nil && maps.Equal(receipt.inbound, inbound)) {
			return ReplayPlan{Packets: receipt.result.Packets, Complete: receipt.result.Complete}, nil
		}
	}
	if h := r.holds[id]; h != nil {
		if !h.replay || h.source != from || h.replaying {
			return ReplayPlan{}, ErrHeld
		}
		return r.configureReplay(h, inbound)
	}
	if len(r.holds) >= r.cfg.MaxHeldSessions {
		return ReplayPlan{}, ErrHoldLimit
	}
	for _, index := range inbound {
		if index >= 1<<48 {
			return ReplayPlan{}, errors.New("relay: inbound SRTP index exceeds 48 bits")
		}
	}
	r.buffer.expire(time.Now())
	h := &sessionHold{id: id, source: from, worker: current, ready: make(chan struct{}), released: make(chan struct{}), acknowledged: true, replay: true, delivered: make(map[uint32]uint64), queuedHigh: make(map[uint32]uint64)}
	r.holds[id] = h
	close(h.ready)
	h.timer = time.AfterFunc(r.cfg.HoldTimeout, func() { r.expireHold(h) })
	s := r.buffer.sessions[id]
	h.replayComplete = s != nil && s.packets.Len() > 0 && !s.untracked
	if s == nil {
		return r.configureReplay(h, inbound)
	}
	s.replayGeneration++
	h.replayGeneration = s.replayGeneration
	h.delivered = maps.Clone(s.replayedThrough)
	h.replayTrimmed = maps.Clone(s.trimmed)
	for item := s.packets.Front(); item != nil; item = item.Next() {
		p := item.Value.(*bufferedPacket)
		if !r.appendReplay(h, heldPacket{caller: p.caller, datagram: p.datagram, ssrc: p.ssrc, index: p.index, media: true}) {
			h.replayComplete = false
		}
	}
	return r.configureReplay(h, inbound)
}

func (r *Relay) configureReplay(h *sessionHold, inbound map[uint32]uint64) (ReplayPlan, error) {
	if inbound == nil {
		return ReplayPlan{Packets: len(h.queue)}, nil
	}
	if h.replayCheckpoint {
		for ssrc, previous := range h.replayInbound {
			if next, known := inbound[ssrc]; !known || next < previous {
				return ReplayPlan{}, ErrHeld
			}
		}
		if maps.Equal(h.replayInbound, inbound) {
			return ReplayPlan{Packets: len(h.queue), Complete: h.replayComplete}, nil
		}
	}
	for _, index := range inbound {
		if index >= 1<<48 {
			return ReplayPlan{}, errors.New("relay: inbound SRTP index exceeds 48 bits")
		}
	}
	s := r.buffer.sessions[h.id]
	// A relay restored mid-call has never observed the earlier wraps. Anchor
	// its locally observed ROC to the checkpoint before comparing the spaces.
	if s != nil {
		for ssrc, floor := range inbound {
			if _, known := h.replayInbound[ssrc]; h.replayCheckpoint && known {
				continue
			}
			translate := indexTranslation(s.indexes[ssrc], floor)
			for i := range h.queue {
				if h.queue[i].media && h.queue[i].ssrc == ssrc {
					h.queue[i].index = translate(h.queue[i].index)
				}
			}
			if trimmed, known := h.replayTrimmed[ssrc]; known {
				h.replayTrimmed[ssrc] = translate(trimmed)
			}
			r.buffer.align(s, ssrc, floor)
		}
	}
	for ssrc, trimmed := range h.replayTrimmed {
		floor, known := inbound[ssrc]
		if !known || trimmed > floor {
			h.replayComplete = false
		}
	}
	firstAfter := make(map[uint32]uint64)
	queue := h.queue
	h.queue = nil
	if h.delivered == nil {
		h.delivered = make(map[uint32]uint64)
	}
	for ssrc, floor := range inbound {
		h.delivered[ssrc] = max(h.delivered[ssrc], floor)
	}
	if s != nil {
		for ssrc, floor := range s.replayedThrough {
			if !h.replayCheckpoint && floor > inbound[ssrc] {
				h.replayComplete = false
			}
			h.delivered[ssrc] = max(h.delivered[ssrc], floor)
		}
	}
	h.replayInbound = maps.Clone(inbound)
	h.replayCheckpoint = true
	for _, p := range queue {
		if floor, known := h.delivered[p.ssrc]; p.media && known && p.index <= floor {
			r.heldPackets--
			r.heldBytes -= packetCost(p.datagram)
			h.bytes -= packetCost(p.datagram)
			r.replayFiltered.Add(1)
			continue
		}
		if first, seen := firstAfter[p.ssrc]; p.media && (!seen || p.index < first) {
			firstAfter[p.ssrc] = p.index
		}
		h.queue = append(h.queue, p)
	}
	// Missing leading input (for example metadata admission before this ring
	// was allocated) must not be advertised as complete codec continuity.
	for ssrc, floor := range h.delivered {
		if s != nil && s.indexes[ssrc] > floor {
			if first, seen := firstAfter[ssrc]; !seen || first != floor+1 {
				h.replayComplete = false
			}
		}
	}
	orderReplay(h.queue)
	clear(h.queuedHigh)
	for _, p := range h.queue {
		if p.media {
			h.queuedHigh[p.ssrc] = max(h.queuedHigh[p.ssrc], p.index)
		}
	}
	return ReplayPlan{Packets: len(h.queue), Complete: h.replayComplete}, nil
}

func indexTranslation(highest, floor uint64) func(uint64) uint64 {
	localFloor := inboundIndex(highest, uint16(floor)) //nolint:gosec // low 16 bits
	return func(index uint64) uint64 {
		if floor >= localFloor {
			return index + floor - localFloor
		}
		return index - min(index, localFloor-floor)
	}
}

func (b *packetBuffer) align(s *bufferedSession, ssrc uint32, floor uint64) {
	highest, known := s.indexes[ssrc]
	if !known {
		return
	}
	translate := indexTranslation(highest, floor)
	s.indexes[ssrc] = translate(highest)
	if trimmed, ok := s.trimmed[ssrc]; ok {
		s.trimmed[ssrc] = translate(trimmed)
	}
	for item := s.packets.Front(); item != nil; item = item.Next() {
		p := item.Value.(*bufferedPacket)
		if p.ssrc == ssrc {
			p.index = translate(p.index)
		}
	}
}

func (r *Relay) enqueueReplay(h *sessionHold, caller netip.AddrPort, packet []byte) {
	p := heldPacket{caller: caller, datagram: make([]byte, MaxHeaderLen+len(packet))}
	copy(p.datagram[MaxHeaderLen:], packet)
	if ssrc, seq, ok := callerIndex(packet); ok {
		p.media, p.ssrc = true, ssrc
		s := r.buffer.sessions[h.id]
		if s == nil {
			r.replayDrop(h)
			return
		}
		if _, tracked := s.indexes[ssrc]; !tracked {
			r.replayDrop(h)
			return
		}
		p.index = inboundIndex(s.indexes[ssrc], seq)
	}
	r.appendReplay(h, p)
}

func (r *Relay) appendReplay(h *sessionHold, p heldPacket) bool {
	if floor, known := h.delivered[p.ssrc]; p.media && known && p.index <= floor {
		r.replayFiltered.Add(1)
		return true
	}
	// Replay uses the cache's per-session byte bound rather than #6's
	// 256-packet move cap. The existing total hold budget bounds all pinned
	// cache references plus new packets, even if the ring evicts during replay.
	if h.bytes+packetCost(p.datagram) > min(r.buffer.cfg.MaxSessionBytes, r.cfg.MaxHeldBytes) || r.heldBytes+packetCost(p.datagram) > r.cfg.MaxTotalHeldBytes {
		r.replayDrop(h)
		return false
	}
	// Most arrivals append in O(1). Only a late packet requires insertion;
	// batches never re-sort or scan the remaining queue.
	if h.replayCheckpoint && p.media && p.index < h.queuedHigh[p.ssrc] {
		at := len(h.queue)
		for i := len(h.queue) - 1; i >= 0; i-- {
			if h.queue[i].media && h.queue[i].ssrc == p.ssrc {
				if h.queue[i].index <= p.index {
					break
				}
				at = i
			}
		}
		h.queue = append(h.queue, heldPacket{})
		copy(h.queue[at+1:], h.queue[at:])
		h.queue[at] = p
	} else {
		h.queue = append(h.queue, p)
	}
	if p.media {
		h.queuedHigh[p.ssrc] = max(h.queuedHigh[p.ssrc], p.index)
	}
	h.bytes += packetCost(p.datagram)
	r.heldBytes += packetCost(p.datagram)
	r.heldPackets++
	return true
}

func (r *Relay) replayDrop(h *sessionHold) {
	r.holdDrops.Add(1)
	h.replayComplete = false
	h.replayResult.Dropped++
}

// Keep the interleaving slots of different SSRCs/control packets, but order
// each stream by its extended inbound index. This tolerates ring reordering
// without advancing the 64-packet receive window past older cached packets.
func orderReplay(queue []heldPacket) {
	streams := make(map[uint32][]heldPacket)
	for _, p := range queue {
		if p.media {
			streams[p.ssrc] = append(streams[p.ssrc], p)
		}
	}
	for _, stream := range streams {
		sort.SliceStable(stream, func(i, j int) bool { return stream[i].index < stream[j].index })
	}
	for i, p := range queue {
		if !p.media {
			continue
		}
		queue[i] = streams[p.ssrc][0]
		streams[p.ssrc] = streams[p.ssrc][1:]
	}
}

// ReplaySession drains the ring AND packets that arrived behind the gate,
// paced on the ordinary private UDP leg. Release is atomic with the final
// empty-queue check, so a live packet can never overtake buffered input.
// The private HTTP API uses this same method; it carries no media keys.
func (r *Relay) ReplaySession(ctx context.Context, id string, to netip.AddrPort) (ReplayResult, error) {
	to = unmap(to)
	if !r.registered(to) {
		return ReplayResult{}, errors.New("relay: replay needs a registered target")
	}
	r.forwardMu.Lock()
	h := r.holds[id]
	if h == nil || !h.replay {
		if s := r.bufferSession(id); s != nil && s.receipt != nil && s.receipt.target == to && r.flows.sessionWorker(id, to) == to {
			result := s.receipt.result
			r.forwardMu.Unlock()
			if result.Expired {
				return result, ErrHoldExpired
			}
			return result, nil
		}
		r.forwardMu.Unlock()
		return ReplayResult{Expired: true}, ErrHoldExpired
	}
	if !h.replayCheckpoint {
		r.forwardMu.Unlock()
		return ReplayResult{}, ErrReplayNotReady
	}
	if h.replaying {
		r.forwardMu.Unlock()
		return ReplayResult{}, ErrReplayInProgress
	}
	if h.worker != to {
		r.forwardMu.Unlock()
		return ReplayResult{}, errors.New("relay: replay target is not the moved owner")
	}
	h.replaying = true
	cfg := r.buffer.cfg
	pacer := r.replayPacing[to]
	if pacer == nil {
		pacer = newReplayPacer(cfg, time.Now())
		r.replayPacing[to] = pacer
	}
	r.forwardMu.Unlock()
	started := time.Now()
	finishLocked := func(err error) (ReplayResult, error) {
		if err != nil {
			h.replayComplete = false
		}
		h.replaying = false
		if errors.Is(err, ErrHoldExpired) {
			h.replayComplete = false
			h.replayResult.Expired = true
		}
		h.replayResult.Duration += time.Since(started)
		r.saveReplay(h)
		return h.replayResult, err
	}
	finish := func(err error) (ReplayResult, error) {
		r.forwardMu.Lock()
		defer r.forwardMu.Unlock()
		return finishLocked(err)
	}
	timer := time.NewTimer(cfg.ReplayInterval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		r.forwardMu.Lock()
		if r.holds[id] != h {
			r.forwardMu.Unlock()
			return finish(ErrHoldExpired)
		}
		r.drainReplayBatch(h, to, pacer)
		if len(h.queue) == 0 {
			// Empty queue release is O(1); normal live input cannot overtake it.
			r.releaseHold(h, to)
			r.replays.Add(1)
			result, err := finishLocked(nil)
			r.forwardMu.Unlock()
			return result, err
		}
		r.forwardMu.Unlock()
		timer.Reset(cfg.ReplayInterval)
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-r.ctx.Done():
			return finish(errors.New("relay: closed"))
		case <-h.released:
			return finish(ErrHoldExpired)
		case <-timer.C:
		}
	}
}

func (r *Relay) bufferSession(id string) *bufferedSession {
	if r.buffer == nil {
		return nil
	}
	return r.buffer.sessions[id]
}

// One shared token bucket per target, protected by forwardMu. Refill follows
// elapsed time; another session cannot reset the target's burst allowance.
type replayPacer struct {
	cfg            BufferConfig
	at             time.Time
	packets, bytes float64
}

func newReplayPacer(cfg BufferConfig, now time.Time) *replayPacer {
	return &replayPacer{cfg: cfg, at: now, packets: float64(cfg.ReplayBatchPackets), bytes: float64(cfg.ReplayBatchBytes)}
}
func (p *replayPacer) take(now time.Time, size int) bool {
	elapsed := max(0, float64(now.Sub(p.at))/float64(p.cfg.ReplayInterval))
	p.at = now
	p.packets = min(float64(p.cfg.ReplayBatchPackets), p.packets+elapsed*float64(p.cfg.ReplayBatchPackets))
	p.bytes = min(float64(p.cfg.ReplayBatchBytes), p.bytes+elapsed*float64(p.cfg.ReplayBatchBytes))
	cost := min(float64(size), float64(p.cfg.ReplayBatchBytes)) // one oversized packet spends a full byte burst
	if p.packets < 1 || p.bytes < cost {
		return false
	}
	p.packets--
	p.bytes -= cost
	return true
}

// drainReplayBatch runs under forwardMu. Its work depends only on this batch,
// even when the remainder contains tens of thousands of packets.
func (r *Relay) drainReplayBatch(h *sessionHold, to netip.AddrPort, pacer *replayPacer) {
	for count := 0; len(h.queue) > 0 && count < r.buffer.cfg.ReplayBatchPackets; count++ {
		p := h.queue[0]
		if !pacer.take(time.Now(), len(p.datagram)) {
			break
		}
		h.queue[0] = heldPacket{}
		h.queue = h.queue[1:]
		h.bytes -= packetCost(p.datagram)
		r.heldBytes -= packetCost(p.datagram)
		r.heldPackets--
		if floor, known := h.delivered[p.ssrc]; p.media && known && p.index <= floor {
			h.replayResult.Filtered++
			r.replayFiltered.Add(1)
			continue
		}
		if p.media {
			h.delivered[p.ssrc] = p.index
			if s := r.buffer.sessions[h.id]; s != nil {
				if s.replayedThrough == nil {
					s.replayedThrough = make(map[uint32]uint64)
				}
				s.replayedThrough[p.ssrc] = max(s.replayedThrough[p.ssrc], p.index)
			}
		}
		if r.sendHeld(h.id, p, to) {
			h.replayResult.Packets++
			r.replayPackets.Add(1)
		} else {
			h.replayComplete = false
			h.replayResult.SendFailures++
			r.holdSendFailures.Add(1)
		}
	}
}
