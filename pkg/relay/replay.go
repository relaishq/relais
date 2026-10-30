package relay

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"time"
)

var (
	ErrBufferDisabled      = errors.New("relay: caller buffer disabled")
	ErrReplayInProgress    = errors.New("relay: replay already running")
	ErrReplayTargetChanged = errors.New("relay: replay target tenure changed")
	ErrReplayNotReady      = errors.New("relay: replay checkpoint not supplied")
)

// ReplayWindow bounds selected caller media in inbound SRTP index space.
// It describes possible input, rather than promising every index in the range.
type ReplayWindow struct {
	SSRC  uint32 `json:"ssrc"`
	First uint64 `json:"first"`
	Last  uint64 `json:"last"`
}

// ReplayPlan reports whether the retained ring covers the checkpoint. An
// incomplete ring can still deliver retained media, followed by PLI recovery.
type ReplayPlan struct {
	Packets  int            `json:"packets"`
	Complete bool           `json:"complete"`
	Windows  []ReplayWindow `json:"windows,omitempty"`
}

// ReplayResult includes loss observed after preparation or worker adoption.
// Dropped counts ring-copy/live admission drops and packets discarded on expiry;
// historical ring gaps clear Complete without inventing a missing-packet count.
type ReplayResult struct {
	Windows      []ReplayWindow `json:"windows,omitempty"`
	Packets      int            `json:"packets"`
	Filtered     int            `json:"filtered"`
	Duration     time.Duration  `json:"duration"`
	Dropped      int            `json:"dropped"`
	SendFailures int            `json:"send_failures"`
	Expired      bool           `json:"expired"`
	Complete     bool           `json:"complete"`
}

// replayReceipt is retained with bounded session metadata, until flow removal.
type replayReceipt struct {
	source, target netip.AddrPort
	tenure         uint64
	inbound        map[uint32]uint64
	result         ReplayResult
}

func (r *Relay) saveReplay(h *sessionHold) {
	h.replayResult.Complete = h.replayComplete && !h.replayResult.Expired
	if h.replayCheckpoint && r.buffer != nil {
		if current := r.holds[h.id]; current != nil && current != h {
			return
		}
		if s := r.buffer.sessions[h.id]; s != nil && s.replayGeneration == h.replayGeneration && (!h.replayTarget.IsValid() || h.replayTarget == h.worker && h.replayTenure == s.routeTenure) {
			s.receipt = &replayReceipt{source: h.source, target: h.worker, tenure: s.routeTenure, inbound: maps.Clone(h.replayInbound), result: cloneReplayResult(h.replayResult)}
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
	if s := r.buffer.sessions[id]; s != nil {
		s.noteReplayTarget(current, 0)
	}
	if s := r.buffer.sessions[id]; s != nil && s.receipt != nil {
		receipt := s.receipt
		if r.holds[id] == nil && receipt.source == from && receipt.target == current && receipt.tenure == s.routeTenure && (inbound == nil && receipt.source != receipt.target || inbound != nil && maps.Equal(receipt.inbound, inbound)) {
			return ReplayPlan{Packets: receipt.result.Packets, Complete: receipt.result.Complete, Windows: slices.Clone(receipt.result.Windows)}, nil
		}
	}
	if h := r.holds[id]; h != nil {
		if h.source != from || h.replaying || !h.acknowledged {
			return ReplayPlan{}, ErrHeld
		}
		if !h.replay {
			if r.barrierEndpoint(from) == from {
				// Ordinary live exports retain their existing uncertainty and
				// same-target confirmation path. Only an authorized NAT drain
				// has lost its old leg and needs the retained pre-barrier input.
				return ReplayPlan{}, ErrHeld
			}
			// Control has fenced the uncertain export before requesting replay.
			// Adopt its planned gate atomically: releasing only its held queue
			// would omit pre-barrier input missing from the recovery checkpoint.
			// A later exact move could then recover those old holes behind live
			// video, reversing the decoder's source clock.
			h.replay = true
			h.queuedHigh = make(map[uint32]uint64)
			for i := range h.queue {
				p := &h.queue[i]
				if ssrc, seq, ok := callerIndex(p.datagram[MaxHeaderLen:]); ok {
					if high, tracked := r.trackedIndex(r.bufferSession(id), ssrc); tracked {
						p.media, p.ssrc, p.index = true, ssrc, inboundIndex(high, seq)
					}
				}
			}
			r.rebuildReplay(h, inbound)
		}
		if s := r.bufferSession(id); h.replayTarget.IsValid() && (h.replayTarget != current || s != nil && h.replayTenure != s.routeTenure) {
			// Keep the gate, but prepare from the new receiver's checkpoint.
			// The old drain's floor cannot suppress input for this tenure.
			for _, index := range inbound {
				if index >= 1<<48 {
					return ReplayPlan{}, errors.New("relay: inbound SRTP index exceeds 48 bits")
				}
			}
			r.rebuildReplay(h, inbound)
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
	h.replayTrimmed = maps.Clone(s.trimmed)
	for item := s.packets.Front(); item != nil; item = item.Next() {
		p := item.Value.(*bufferedPacket)
		if !r.appendReplay(h, heldPacket{caller: p.caller, datagram: p.datagram, ssrc: p.ssrc, index: p.index, media: true}) {
			h.replayComplete = false
		}
	}
	return r.configureReplay(h, inbound)
}

// rebuildReplay runs under forwardMu. Ring eviction must not discard live
// packets still owned by the gate; merge both sets without duplicating input.
func (r *Relay) rebuildReplay(h *sessionHold, inbound map[uint32]uint64) {
	r.buffer.expire(time.Now())
	gated := h.queue
	r.heldPackets -= len(gated)
	r.heldBytes -= h.bytes
	h.queue, h.bytes = nil, 0
	h.delivered = make(map[uint32]uint64)
	h.replayInbound, h.replayTrimmed = nil, nil
	h.replayCheckpoint = false
	h.replayTarget, h.replayTenure = netip.AddrPort{}, 0
	h.replayResult = ReplayResult{}
	clear(h.queuedHigh)
	s := r.bufferSession(h.id)
	h.replayComplete = s != nil && s.packets.Len() > 0 && !s.untracked
	type packetIndex struct {
		ssrc  uint32
		index uint64
	}
	seen := make(map[packetIndex]bool)
	appendPacket := func(p heldPacket) {
		if floor, known := inbound[p.ssrc]; p.media && known && p.index <= floor {
			r.replayFiltered.Add(1)
			return
		}
		key := packetIndex{p.ssrc, p.index}
		if p.media && seen[key] {
			return
		}
		if p.media {
			seen[key] = true
		}
		if !r.appendReplay(h, p) {
			h.replayComplete = false
		}
	}
	if s != nil {
		// Align before filtering so checkpoint ROC changes use one index space.
		for ssrc, floor := range inbound {
			translate := anchorIndex(floor)
			for i := range gated {
				if gated[i].media && gated[i].ssrc == ssrc {
					gated[i].index = translate(gated[i].index)
				}
			}
			r.buffer.align(s, ssrc, floor)
		}
		s.replayGeneration++
		h.replayGeneration = s.replayGeneration
		h.replayTrimmed = maps.Clone(s.trimmed)
	}
	// Retain the live input the gate already owns before copying the ring.
	for _, p := range gated {
		appendPacket(p)
	}
	if s != nil {
		for item := s.packets.Front(); item != nil; item = item.Next() {
			p := item.Value.(*bufferedPacket)
			appendPacket(heldPacket{caller: p.caller, datagram: p.datagram, ssrc: p.ssrc, index: p.index, media: true})
		}
	}
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
			return replayPlan(h), nil
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
			translate := anchorIndex(floor)
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
	return replayPlan(h), nil
}

// anchorIndex re-extends a retained caller index around a worker's
// authenticated anchor, within the RFC 3711 half-space envelope. A relay
// restored mid-call starts its own ROC at zero and cannot represent a
// sequence observed before a wrap it never saw, so no single offset can
// align a reordered cold ring (seq 0 then 65534 around anchor 65535). Each
// retained index is rebuilt from its own 16-bit sequence instead.
func anchorIndex(anchor uint64) func(uint64) uint64 {
	return func(index uint64) uint64 { return inboundIndex(anchor, uint16(index)) } //nolint:gosec // low 16 bits
}

// align rebuilds one SSRC's retained indexes, trims and high water mark in
// the anchor's index space. Retained packets are within the ring window, far
// inside the half-space envelope, so a warm ring keeps the same indexes.
// The lowest observed index is lifetime metadata, possibly many rollovers
// old: it moves by the high water mark's correction instead of being
// re-extended from its 16 bits, which would misplace it into the anchor's
// epoch. A cold ring can only under-label indexes, so it stays a lower bound.
func (b *packetBuffer) align(s *bufferedSession, ssrc uint32, anchor uint64) {
	highest, known := s.indexes[ssrc]
	if !known {
		return
	}
	translate := anchorIndex(anchor)
	aligned := translate(highest)
	if lowest, ok := s.lowest[ssrc]; ok {
		if aligned >= highest {
			s.lowest[ssrc] = lowest + (aligned - highest)
		} else {
			s.lowest[ssrc] = lowest - min(lowest, highest-aligned)
		}
	}
	highest = aligned
	if trimmed, ok := s.trimmed[ssrc]; ok {
		s.trimmed[ssrc] = translate(trimmed)
	}
	for item := s.packets.Front(); item != nil; item = item.Next() {
		p := item.Value.(*bufferedPacket)
		if p.ssrc == ssrc {
			p.index = translate(p.index)
			highest = max(highest, p.index)
		}
	}
	s.indexes[ssrc] = highest
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
	// Replay uses the hold byte bound rather than #6's 256-packet move cap.
	// The default allows a full ring plus gated live traffic. The total hold
	// budget accounts for both even if the ring evicts during replay.
	if h.bytes+packetCost(p.datagram) > r.cfg.MaxHeldBytes || r.heldBytes+packetCost(p.datagram) > r.cfg.MaxTotalHeldBytes {
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
	r.replayDrops.Add(1)
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
		if s := r.bufferSession(id); s != nil && s.receipt != nil && s.receipt.target == to && s.receipt.tenure == s.routeTenure && r.flows.sessionWorker(id, to) == to {
			result := cloneReplayResult(s.receipt.result)
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
	if s := r.bufferSession(id); s != nil {
		s.noteReplayTarget(to, 0)
		if h.replayTarget.IsValid() && (h.replayTarget != to || h.replayTenure != s.routeTenure) {
			r.forwardMu.Unlock()
			return ReplayResult{}, ErrReplayTargetChanged
		}
		firstDrain := !h.replayTarget.IsValid()
		h.replayTarget, h.replayTenure = to, s.routeTenure
		// Only this receiver tenure can have authenticated these indexes.
		// A new target must replay everything after its own checkpoint.
		if s.replayedTarget == to && s.replayedTenure == h.replayTenure {
			for ssrc, floor := range s.replayedThrough {
				if firstDrain && floor > h.replayInbound[ssrc] {
					h.replayComplete = false
				}
				h.delivered[ssrc] = max(h.delivered[ssrc], floor)
			}
		}
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
		// Cancellation leaves the queue intact. Only actual loss or expiry
		// clears completeness, so a lossless retry can preserve codec continuity.
		h.replaying = false
		if errors.Is(err, ErrHoldExpired) || r.ctx.Err() != nil {
			h.replayComplete = false
			if !h.replayResult.Expired {
				r.markReplayExpired(h)
			}
		}
		h.replayResult.Duration += time.Since(started)
		r.saveReplay(h)
		return cloneReplayResult(h.replayResult), err
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
		if h.worker != to || r.bufferSession(id) != nil && r.bufferSession(id).routeTenure != h.replayTenure {
			h.replayComplete = false
			result, err := finishLocked(ErrReplayTargetChanged)
			r.forwardMu.Unlock()
			return result, err
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
			return finish(ErrHoldExpired)
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
		floor, known := h.delivered[p.ssrc]
		filtered := p.media && known && p.index <= floor
		if !filtered && !pacer.take(time.Now(), len(p.datagram)) {
			break
		}
		h.queue[0] = heldPacket{}
		h.queue = h.queue[1:]
		h.bytes -= packetCost(p.datagram)
		r.heldBytes -= packetCost(p.datagram)
		r.heldPackets--
		if filtered {
			h.replayResult.Filtered++
			r.replayFiltered.Add(1)
			continue
		}
		if r.sendHeld(h.id, p, to) {
			r.noteReplayDelivery(h, p, to)
			h.replayResult.Packets++
			if p.media {
				h.replayResult.Windows = noteReplayWindow(h.replayResult.Windows, p)
			}
			r.replayPackets.Add(1)
		} else {
			h.replayComplete = false
			h.replayResult.SendFailures++
			r.replaySendFailures.Add(1)
			r.holdSendFailures.Add(1)
		}
	}
}

// noteReplayDelivery is called only after a successful send, under forwardMu.
func (r *Relay) noteReplayDelivery(h *sessionHold, p heldPacket, to netip.AddrPort) {
	if !p.media {
		return
	}
	h.delivered[p.ssrc] = max(h.delivered[p.ssrc], p.index)
	if s := r.bufferSession(h.id); s != nil {
		s.noteReplayTarget(to, 0)
		if s.replayedTarget != to || s.replayedTenure != s.routeTenure {
			s.replayedTarget, s.replayedTenure = to, s.routeTenure
			s.replayedThrough = make(map[uint32]uint64)
		}
		s.replayedThrough[p.ssrc] = max(s.replayedThrough[p.ssrc], p.index)
	}
}

func cloneReplayResult(result ReplayResult) ReplayResult {
	result.Windows = slices.Clone(result.Windows)
	return result
}

// Bounds describe selected media, before resume; newly gated traffic after
// preparation cannot have been processed by the fenced source. Result windows
// refine those bounds to media actually sent. Gaps within a range are possible.
func replayPlan(h *sessionHold) ReplayPlan {
	plan := ReplayPlan{Packets: len(h.queue), Complete: h.replayComplete}
	for _, p := range h.queue {
		if p.media {
			plan.Windows = noteReplayWindow(plan.Windows, p)
		}
	}
	return plan
}
func noteReplayWindow(windows []ReplayWindow, p heldPacket) []ReplayWindow {
	for i := range windows {
		if windows[i].SSRC == p.ssrc {
			windows[i].First = min(windows[i].First, p.index)
			windows[i].Last = max(windows[i].Last, p.index)
			return windows
		}
	}
	return append(windows, ReplayWindow{SSRC: p.ssrc, First: p.index, Last: p.index})
}

func (r *Relay) markReplayExpired(h *sessionHold) {
	if h.replayResult.Expired {
		return
	}
	h.replayComplete = false
	h.replayResult.Expired = true
	h.replayResult.Dropped += len(h.queue)
	r.replayDrops.Add(uint64(len(h.queue)))
	r.replayExpiries.Add(1)
}

// A trusted route change starts a fresh receiver tenure. Idempotent move
// confirmations keep the same tenure, even after the gate has been released.
func (s *bufferedSession) noteReplayTarget(target netip.AddrPort, epoch uint64) {
	if s.routeTarget != target || epoch != 0 && s.routeEpoch != epoch {
		s.routeTarget, s.routeEpoch = target, epoch
		s.routeTenure++
	}
}
