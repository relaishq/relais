package mediaworker

import (
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Export cost includes immutable payload ownership, frozen record/reference
// headers and the growing JSON buffer plus returned encoding. Ingress is not
// exported. Its separate bounded reservation remains in the live mailbox.
// bridgeMoveCost is the single reservation model for credit, admission,
// draining and export. Owned includes two request copies and terminal capacity.
// Only one request copy needs the encoder/frozen-image copy allowance.
func (s *session) bridgeMoveCost(owned, requests int64) (total, scratch int64) {
	b := s.agent.v2.bridge
	fixed := 2*int64(s.snapshotLimits().MaxBytes+max(len(s.agent.latest.History), s.agent.v2.registration.Bounds.MaxHistoryBytes)) + 2*bridgeMetadataPartition + 512
	// Reserve future actor growth even when observers have not started yet.
	// Mailbox includes all pending results AND outstanding terminal reservations.
	worst := int64(10*b.limits.ObserverRequestBytes + 9*(b.limits.ObserverMailboxBytes+b.limits.AdapterMoveOnlyBytes))
	actor := max(owned+8*max(0, owned-requests), worst)
	scratch = actor - owned + b.recordCount.Load()*bridgeRecordCharge + 3*(b.encodingBytes.Load()+fixed)
	return owned + b.recordMemory.Load() + scratch, scratch
}
func (s *session) bridgeExportCostLocked() int64 {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	total, _ := s.bridgeMoveCost(s.moveReserved, s.moveRequestBytes.Load())
	return total
}
func (s *session) bridgeLookaheadBudget() int64 {
	b := s.agent.v2.bridge
	return int64(min(s.snapshotLimits().MoveBytesPerSession, s.agent.v2.registration.Bounds.MaxMailboxBytes) - b.limits.ActorReserveBytes - b.limits.ExportHeadroomBytes)
}
func bridgePacketExportCost(r bridgePacketRecord) int64 {
	return 2*bridgeRecordCharge + int64(len(r.Payload)) + 3*int64(bridgeRecordJSONBytes(r)+bridgeRecordQueueJSONBytes(r)+2)
}

// Credits cannot know future digests or turn IDs. Bound those fields using
// their actual JSON representation, and charge payload base64 separately.
func bridgeCreditRecordCost(scope BridgeOutputScope, seq uint64) int64 {
	r := bridgePacketRecord{Scope: scope, Duration: 120 * time.Millisecond, Length: bridgeBinaryBytes, Stream: ^uint64(0)}
	r.Receipt = BridgeOutputReceipt{BridgeOutputScope: scope, Sequence: seq, Turn: ^agent.Turn(0), Revision: ^uint64(0), Outcome: "refused", Reason: "invalid_audio", Consumed: true}
	for i := range r.Digest {
		r.Digest[i] = 255
	}
	// base64 rounds each payload to a multiple of four; 8 B covers its
	// triple-buffer padding. Payload memory + triple base64 costs <=5 B/B.
	return bridgePacketExportCost(r) + 3*int64(len(`,"payload":""`)) + 8
}
func (s *session) bridgeLookaheadCreditLocked(slots, bytes int) (int, int) {
	b := s.agent.v2.bridge
	pending := int(b.reservedPackets.Load())
	fixed := bridgeCreditRecordCost(BridgeOutputScope(b.state.Scope), b.state.Through+uint64(b.limits.PendingPackets))
	available := s.bridgeLookaheadBudget() - s.bridgeExportCostLocked() - int64(pending)*fixed - 5*b.reservedBytes.Load()
	duration := b.limits.AudioDuration - b.audioDuration - time.Duration(pending)*120*time.Millisecond
	slots = min(slots, int(duration/(120*time.Millisecond)))
	if b.draining > 0 || slots <= 0 || available <= fixed {
		return 0, 0
	}
	n := max(1, min(slots, int(available/(fixed+5*bridgeBinaryBytes))))
	bytes = min(bytes, int((available-int64(n)*fixed)/5))
	if bytes <= 0 {
		return 0, 0
	}
	return n, bytes
}
func (b *bridgeOutputHost) signalChange() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}
func (s *session) holdBridgeCredit() func() {
	b := s.agent.v2.bridge
	s.mu.Lock()
	b.draining++
	s.mu.Unlock()
	return func() { s.mu.Lock(); b.draining--; b.signalChange(); s.mu.Unlock() }
}

// The actor may wait here, while ingress, playback and checkpoint settlement
// keep running. A failed wait has neither paused input nor stopped tasks.
func (s *session) drainBridgeToFit(deadline time.Time) error {
	b := s.agent.v2.bridge
	timer := time.NewTimer(max(0, time.Until(deadline)))
	defer timer.Stop()
	for {
		if !time.Now().Before(deadline) {
			return ErrExportNotFlushed
		}
		s.mu.Lock()
		fits := b.reservedPackets.Load() == 0 && s.bridgeExportCostLocked() <= s.bridgeLookaheadBudget()
		s.mu.Unlock()
		if fits {
			return nil
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-timer.C:
			return ErrExportNotFlushed
		case <-b.changed:
		}
	}
}
