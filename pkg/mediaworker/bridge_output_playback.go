package mediaworker

import (
	"fmt"
	"slices"
	"time"

	"github.com/pion/rtp"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func (p playbackRuntime) bridgeSnapshot() playbackState { return p.bridgeSnapshotWith(nil) }
func (p playbackRuntime) bridgeSnapshotWith(extra *bridgePacketRecord) playbackState {
	state := playbackState{NextAt: p.nextAt, NextTS: p.nextTS, Initialized: p.initialized, Pending: len(p.bridgeQueue), Length: p.length, Duration: p.duration, Notify: slices.Clone(p.notify)}
	// An empty Notify slice must encode like nil. Otherwise omitzero emits
	// pb:{} which decodes as zero and fails canonical component validation.
	if len(state.Notify) == 0 {
		state.Notify = nil
	}
	var duration time.Duration
	bytes := 0
	for _, q := range p.bridgeQueue {
		if duration+q.Duration > playbackWindowDuration {
			break
		}
		kind := "bridge"
		n := uint32(1)
		if q.isGap() {
			kind = "gap"
			n = uint32(q.Last - q.Sequence + 1)
		} else {
			r, ok := p.bridge.records[q.Sequence]
			if extra != nil && extra.Receipt.Sequence == q.Sequence {
				r = *extra
				ok = true
			}
			if !ok {
				break
			}
			bytes += len(r.Payload)
			if bytes > playbackWindowBytes {
				break
			}
		}
		// The bridge prefix admits multiple tagged spans. Metadata, rather than
		// queue capacity, limits this crash reference prefix to its reserved space.
		if len(state.Window) >= 2 {
			break
		}
		state.Window = append(state.Window, audioSpan{Kind: kind, Stream: q.Stream, Sequence: q.Sequence, Turn: q.Turn, Count: n, GapDuration: func() time.Duration {
			if q.isGap() {
				return q.Duration
			}
			return 0
		}()})
		duration += q.Duration
	}
	return state
}

// Called by the existing pacer with session.mu held. The same send fence is
// used by interruption and the eventual generation replacement transaction.
func (s *session) paceBridgeLocked() (time.Duration, error) {
	p := &s.agent.v2.playback
	b := s.agent.v2.bridge
	defer b.signalChange()
	if b.state == nil || len(p.bridgeQueue) == 0 || (s.lease.Epoch > 0 && b.state.Scope.Epoch != s.lease.Epoch) {
		return time.Hour, nil
	}
	v := cloneBridgeState(b.state)
	now := s.taskClockLocked()
	if !p.initialized {
		p.nextTS = s.state.Audio.InitialTS
		p.initialized = true
		p.nextAt = now
	}
	if v.RebaseTicks > 0 && p.bridgeQueue[0].Sequence > v.RebaseThrough {
		v.RebaseTicks = 0
		v.RebaseThrough = 0
	}
	// Preserve #265's late-wake policy. Record only this rebase's media-time
	// increment so a subsequent gap can consume it once, never nextAt twice.
	if now.Sub(p.nextAt) >= playbackRebaseAfter {
		v.RebaseThrough = v.Through
		v.RebaseTicks += uint64(rtpClockTicks(now.Sub(p.nextAt)))
		p.rebase(now)
	}
	for len(p.bridgeQueue) > 0 {
		q := p.bridgeQueue[0]
		if q.isGap() {
			ticks := uint64(rtpClockTicks(q.Duration))
			covered := uint64(0)
			if q.Last <= v.RebaseThrough {
				covered = min(ticks, v.RebaseTicks)
			}
			v.RebaseTicks -= covered
			p.nextTS += uint32(ticks - covered)
			p.bridgeQueue = p.bridgeQueue[1:]
			advanceBridgeSent(b, v)
			b.state = v
			continue
		}
		r, ok := b.records[q.Sequence]
		if !ok || len(r.Payload) == 0 || r.Stream != q.Stream || r.Receipt.Turn != q.Turn {
			return 0, fmt.Errorf("%w: bridge paced reference", agent.ErrIncompatibleAgent)
		}
		if r.Scope != BridgeOutputScope(v.Scope) || q.Turn < max(v.CancelFloor, v.ClearFloor) {
			s.retireBridgePacketLocked(q.Sequence, "canceled", v)
			p.bridgeQueue = p.bridgeQueue[1:]
			continue
		}
		if now.Before(p.nextAt) {
			b.state = v
			s.projectBridgeLocked(s.state.Agent.V2)
			return p.nextAt.Sub(now), nil
		}
		// Scope, turn and receipt identity are checked at the wire boundary. The
		// epoch remap in phase B must occur under this same lock.
		if r.Receipt.Outcome != "admitted" {
			return 0, fmt.Errorf("%w: bridge send outcome", agent.ErrIncompatibleAgent)
		}
		header := rtp.Header{Version: 2, PayloadType: s.state.Audio.PayloadType, SSRC: s.state.Audio.SSRC, Timestamp: p.nextTS}
		if err := s.sendAgentAudioLocked(header, r.Payload, s.state.Agent); err != nil {
			return 0, err
		}
		p.nextAt = p.nextAt.Add(r.Duration)
		p.nextTS += rtpClockTicks(r.Duration)
		// Ordinary audio after a gap starts a new interval. Old rebase coverage
		// cannot be reused against a later, unrelated gap.
		if q.Sequence <= v.RebaseThrough {
			v.RebaseTicks -= min(v.RebaseTicks, uint64(rtpClockTicks(r.Duration)))
		}
		p.bridgeQueue = p.bridgeQueue[1:]
		s.retireBridgePacketLocked(q.Sequence, "sent", v)
		v.Revision++
		b.state = v
		s.projectBridgeLocked(s.state.Agent.V2)
		return max(0, p.nextAt.Sub(s.taskClockLocked())), nil
	}
	v.Revision++
	b.state = v
	s.projectBridgeLocked(s.state.Agent.V2)
	return time.Hour, nil
}
func (s *session) retireBridgePacketLocked(seq uint64, outcome string, v *bridgeOutputState) {
	b := s.agent.v2.bridge
	p := &s.agent.v2.playback
	r := b.records[seq]
	old := r
	r.Receipt.Outcome = outcome
	r.Receipt.Revision = v.Revision + 1
	p.length -= len(r.Payload)
	p.duration -= r.Duration
	b.audioBytes -= len(r.Payload)
	b.audioDuration -= r.Duration
	_ = s.reserveBridgeMemory(-int64(len(r.Payload)))
	r.Payload = nil
	b.replaceRecordEncoding(&old, &r)
	b.records[seq] = r
	advanceBridgeSent(b, v)
}
func advanceBridgeSent(b *bridgeOutputHost, v *bridgeOutputState) {
	// Sent is the contiguous locally terminal prefix, including refused gaps.
	for next := v.Sent + 1; next <= v.Through; next++ {
		record, ok := b.records[next]
		if !ok || record.Receipt.Outcome == "admitted" {
			break
		}
		v.Sent = next
	}
}
