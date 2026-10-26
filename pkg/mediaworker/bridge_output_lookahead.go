package mediaworker

import (
	"encoding/base64"
	"time"
	"unsafe"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Frozen record and queue-reference headers, rounded up to a cache line. The
// 512-byte live map charge stays unchanged; payload storage is shared.
const bridgeFrozenRecordBytes = int64((unsafe.Sizeof(bridgePacketRecord{}) + unsafe.Sizeof(bridgeQueued{}) + 63) &^ 63)

// bridgeJSONBuffers bounds the growing encoding/json buffer and its returned
// copy: max(old+new during growth, buffer+returned) <= 3*encoded bytes.
// The fixed term below adds 16 KiB for allocator page rounding and small buffers.
// Bridge capture encodes the component envelope directly, avoiding a second
// whole-snapshot MarshalJSON buffer. This is a logical reservation, not Go RSS
// (which also includes reusable encoder pools and garbage awaiting collection).
func bridgeJSONBuffers(encoded int64) int64 { return 3 * encoded }

func bridgeBase64Bytes(raw int64) int64 {
	return int64(base64.StdEncoding.EncodedLen(int(raw)))
}

// Requests have two live copies, one frozen copy and one base64 encoding.
// Mailbox is ALREADY charged as escaped JSON by taskResultBytes (including
// terminal space); its live/frozen copies each fit that charge. Adapter bytes
// have one live reservation, one frozen owned copy and one base64 encoding.
func (s *session) bridgeActorMoveCost(owned, requests int64) int64 {
	b := s.agent.v2.bridge
	r := max(requests, int64(b.limits.ObserverRequestBytes))
	m, a := int64(b.limits.ObserverMailboxBytes), int64(b.limits.AdapterMoveOnlyBytes)
	live := max(owned, 2*r+m+a)
	frozen := r + m + a
	// Each task request rounds base64 independently. Four bytes per possible
	// task cover quotes and fragment padding, including retired observer tasks.
	encoded := bridgeBase64Bytes(r) + int64(4*s.agent.v2.registration.Bounds.MaxActiveTasks) + m + bridgeBase64Bytes(a)
	return max(int64(b.limits.ActorReserveBytes), live+frozen+bridgeJSONBuffers(encoded))
}

// encodeByteSlice/appendString may build a temporary fragment before copying
// it into the main buffer. slices.Grow can double its capacity; reserve that
// fragment separately from old/new main buffers. Raw requests and adapter state
// are base64; mailbox already includes its worst escaped JSON. Host custom
// marshalers are bounded by the host metadata cap. The 16 KiB fixed margin
// covers small-buffer minima and allocator page rounding.
func (s *session) bridgeEncodingTemporaryBytes() int64 {
	b := s.agent.v2.bridge
	budgets := s.checkpoint.componentBudgets()
	fragment := max(bridgeBase64Bytes(int64(b.limits.ObserverRequestBytes))+4,
		int64(b.limits.ObserverMailboxBytes), bridgeBase64Bytes(int64(b.limits.AdapterMoveOnlyBytes))+2,
		bridgeBase64Bytes(int64(max(budgets.Hot, budgets.Setup, budgets.History)))+2,
		int64(v2HostMetadataBytes))
	return 2 * fragment
}

func (s *session) bridgeFixedMoveCost() (owned, encoded int64) {
	budgets := s.checkpoint.componentBudgets()
	bounds := s.agent.v2.registration.Bounds
	// cloneAgentState, componentExportState's history clone, binary components,
	// DTLS serialization and small host/state/header copies. Component caps include
	// framing and crypto overhead, so Total safely bounds the plaintext buffers.
	owned = int64(bounds.MaxStateBytes+2*bounds.MaxHistoryBytes+budgets.Total+budgets.Setup+4*v2HostMetadataBytes) + (16 << 10) + s.bridgeEncodingTemporaryBytes()
	// Four binary components, two frozen bridge metadata projections, and 2 KiB
	// of envelope/task framing. Host metadata inside transport is already in Total.
	encoded = bridgeBase64Bytes(int64(budgets.Total)) + 16 + 2*bridgeMetadataPartition + v2HostMetadataBytes
	return owned, encoded
}

// One model serves credit, admission, drain-to-fit and export reservation.
// ActorReserveBytes is a FLOOR on the actor term, not an additional deduction.
// Immutable bridge payloads have one live owner; frozen records share them.
func (s *session) bridgeMoveCost(owned, requests int64) (total, scratch int64) {
	b := s.agent.v2.bridge
	fixedOwned, fixedJSON := s.bridgeFixedMoveCost()
	actor := s.bridgeActorMoveCost(owned, requests)
	scratch = actor - owned + fixedOwned + b.recordCount.Load()*bridgeFrozenRecordBytes + bridgeJSONBuffers(b.encodingBytes.Load()+fixedJSON)
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
	return int64(min(s.snapshotLimits().MoveBytesPerSession, s.agent.v2.registration.Bounds.MaxMailboxBytes) - b.limits.ExportHeadroomBytes)
}
func bridgePacketExportCost(r bridgePacketRecord) int64 {
	return bridgeRecordCharge + bridgeFrozenRecordBytes + int64(len(r.Payload)) + bridgeJSONBuffers(int64(bridgeRecordJSONBytes(r)+bridgeRecordQueueJSONBytes(r)+2))
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
