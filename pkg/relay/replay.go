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

type ReplayResult struct {
	Packets  int           `json:"packets"`
	Filtered int           `json:"filtered"`
	Duration time.Duration `json:"duration"`
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
	h := &sessionHold{id: id, source: from, worker: from, ready: make(chan struct{}), released: make(chan struct{}), acknowledged: true, replay: true, delivered: make(map[uint32]uint64)}
	r.holds[id] = h
	close(h.ready)
	h.timer = time.AfterFunc(r.cfg.HoldTimeout, func() { r.expireHold(h) })
	s := r.buffer.sessions[id]
	h.replayComplete = s != nil && s.packets.Len() > 0 && !s.untracked
	if s == nil {
		return r.configureReplay(h, inbound)
	}
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
		if !maps.Equal(h.replayInbound, inbound) {
			return ReplayPlan{}, ErrHeld
		}
		return ReplayPlan{Packets: len(h.queue), Complete: h.replayComplete}, nil
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
	h.delivered = maps.Clone(inbound)
	h.replayInbound = maps.Clone(inbound)
	h.replayCheckpoint = true
	for _, p := range queue {
		if floor, known := inbound[p.ssrc]; p.media && known && p.index <= floor {
			r.heldPackets--
			r.heldBytes -= len(p.datagram)
			h.bytes -= len(p.datagram)
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
	for ssrc, floor := range inbound {
		if s != nil && s.indexes[ssrc] > floor {
			if first, seen := firstAfter[ssrc]; !seen || first != floor+1 {
				h.replayComplete = false
			}
		}
	}
	orderReplay(h.queue)
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
			r.holdDrops.Add(1)
			return
		}
		if _, tracked := s.indexes[ssrc]; !tracked {
			r.holdDrops.Add(1)
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
	if h.bytes+len(p.datagram) > min(r.buffer.cfg.MaxSessionBytes, r.cfg.MaxHeldBytes) || r.heldBytes+len(p.datagram) > r.cfg.MaxTotalHeldBytes {
		r.holdDrops.Add(1)
		return false
	}
	h.queue = append(h.queue, p)
	h.bytes += len(p.datagram)
	r.heldBytes += len(p.datagram)
	r.heldPackets++
	return true
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
		r.forwardMu.Unlock()
		return ReplayResult{}, ErrHoldExpired
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
	r.forwardMu.Unlock()
	defer func() { r.forwardMu.Lock(); h.replaying = false; r.forwardMu.Unlock() }()
	started := time.Now()
	result := ReplayResult{}
	timer := time.NewTimer(cfg.ReplayInterval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			result.Duration = time.Since(started)
			return result, err
		}
		r.forwardMu.Lock()
		if r.holds[id] != h {
			r.forwardMu.Unlock()
			result.Duration = time.Since(started)
			return result, ErrHoldExpired
		}
		orderReplay(h.queue)
		bytes, count := 0, 0
		for len(h.queue) > 0 && count < cfg.ReplayBatchPackets {
			p := h.queue[0]
			if count > 0 && bytes+len(p.datagram) > cfg.ReplayBatchBytes {
				break
			}
			h.queue[0] = heldPacket{}
			h.queue = h.queue[1:]
			h.bytes -= len(p.datagram)
			r.heldBytes -= len(p.datagram)
			r.heldPackets--
			count++
			bytes += len(p.datagram)
			if floor, known := h.delivered[p.ssrc]; p.media && known && p.index <= floor {
				result.Filtered++
				r.replayFiltered.Add(1)
				continue
			}
			if p.media {
				h.delivered[p.ssrc] = p.index
			}
			if r.sendHeld(h.id, p, to) {
				result.Packets++
				r.replayPackets.Add(1)
			} else {
				r.holdSendFailures.Add(1)
			}
		}
		if len(h.queue) == 0 {
			r.releaseHold(h, to)
			r.replays.Add(1)
			r.forwardMu.Unlock()
			result.Duration = time.Since(started)
			return result, nil
		}
		r.forwardMu.Unlock()
		timer.Reset(cfg.ReplayInterval)
		select {
		case <-ctx.Done():
			result.Duration = time.Since(started)
			return result, ctx.Err()
		case <-r.ctx.Done():
			result.Duration = time.Since(started)
			return result, errors.New("relay: closed")
		case <-h.released:
			result.Duration = time.Since(started)
			return result, ErrHoldExpired
		case <-timer.C:
		}
	}
}
