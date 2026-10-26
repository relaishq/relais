package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func retainBridgeStream(t *testing.T, s *session, scope BridgeOutputScope, packets, size int, wire *dtlsWireConn) {
	t.Helper()
	// This loop measures capacity, not the real-time admission deadline.
	s.agent.v2.bridge.limits.AdmissionTimeout = time.Hour
	for seq := 1; seq <= packets; seq++ {
		request := bridgeRequest(scope, uint64(seq), time.Now())
		request.data = bridgePacket(size)
		binary.BigEndian.PutUint64(request.data[1:9], uint64(seq))
		request.digest = sha256.Sum256(request.data)
		receipt := s.admitBridgePacket(request)
		require.Equal(t, "admitted", receipt.Outcome, "packet %d: %+v", seq, receipt)
		paceBridge(t, s)
		<-wire.packets
	}
}

func TestBridgeOutputRetainsStoreStallCapacity(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			retainBridgeStream(t, s, scope, 1500, size, rig.wire)
			b := s.agent.v2.bridge
			require.Len(t, b.settled, 6)
			require.Len(t, b.records, bridgeRecentSent)
			require.LessOrEqual(t, s.bridgeExportCostLocked(), s.bridgeLookaheadBudget())
			checkpoint, err := s.worker.BridgeOutputCheckpointed(scope)
			require.Error(t, err)
			require.Empty(t, checkpoint)
			require.False(t, s.agent.failed.Load())
			require.NoError(t, s.ctx.Err())
			maximumBridgeObservers(t, rig)
			growBridgeActorEnvelope(t, rig)
			s.mu.Lock()
			total, scratch := s.bridgeMoveCost(s.moveReserved, s.moveRequestBytes.Load())
			require.NoError(t, s.reserveV2ExportScratch())
			require.Equal(t, total, s.moveReserved+b.memory.Load()+scratch)
			s.agent.paused = true
			s.mu.Unlock()
			image, err := s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			encoded, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio, Bytes: image.payload})
			require.NoError(t, err)
			require.NoError(t, validateExactWire(encoded, true))
			require.GreaterOrEqual(t, scratch, bridgeFrozenImageBytes(image)+3*int64(len(encoded)))
			require.LessOrEqual(t, total, s.bridgeLookaheadBudget())
			t.Logf("size=%d retained=1500 packed=%d live=%d encoded=%d modeled=%d allowance=%d", size, bridgePackedBytes(b.settled), b.memory.Load(), len(encoded), total, s.bridgeLookaheadBudget())
		})
	}
}

func TestBridgeOutputPackedReceiptsPreserveExactCut(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	retainBridgeStream(t, s, scope, 1500, 160, rig.wire)
	image := s.exportBridgeLocked()
	saved := cloneAgentState(s.state.Agent)
	wire, err := json.Marshal(image)
	require.NoError(t, err)
	target, _ := newBridgeRig(t, false)
	dst := target.s
	restore := func(image *bridgeOutputMove) error {
		dst.agent.v2.bridge = newBridgeOutputHost(BridgeOutputLimits{})
		dst.agent.v2.playback.bridge = dst.agent.v2.bridge
		dst.agent.latest = cloneAgentState(saved)
		dst.state.Agent = cloneAgentState(saved)
		return dst.restoreBridgeOutput(&movePayload{Bridge: image})
	}
	require.NoError(t, restore(image))
	require.Equal(t, image.Settled, dst.exportBridgeLocked().Settled)
	require.Equal(t, dst.agent.v2.bridge.memory.Load(), dst.agent.v2.bridge.recordMemory.Load()+dst.agent.v2.bridge.settledMemory())
	for _, seq := range []uint64{1, 800, 1468, 1500} {
		before, err := s.worker.BridgeOutputStatus(scope, seq)
		require.NoError(t, err)
		after, err := dst.worker.BridgeOutputStatus(scope, seq)
		require.NoError(t, err)
		require.Equal(t, before, after)
		request := bridgeRequest(scope, seq, time.Now())
		binary.BigEndian.PutUint64(request.data[1:9], seq)
		request.digest = sha256.Sum256(request.data)
		require.Equal(t, *after.Receipt, dst.admitBridgePacket(request))
		request.data[10]++
		request.digest = sha256.Sum256(request.data)
		require.Equal(t, "conflict", dst.admitBridgePacket(request).Reason)
	}
	require.Empty(t, dst.agent.v2.playback.bridgeQueue, "sent identities cannot replay")
	status, err := dst.worker.BridgeOutputStatus(scope, 1501)
	require.NoError(t, err)
	require.Equal(t, "unadmitted", status.Status)
	for _, mutation := range []string{"digest", "chain", "seed", "turn", "revision", "truncate", "coverage", "overlap", "v1-with-packed"} {
		t.Run(mutation, func(t *testing.T) {
			var bad bridgeOutputMove
			require.NoError(t, json.Unmarshal(wire, &bad))
			switch mutation {
			case "digest":
				bad.Settled[0].Data[0]++
			case "chain":
				bad.Settled[0].Chain[0]++
			case "seed":
				bad.Settled[0].Seed[0]++
			case "turn":
				bad.Settled[0].Turn++
			case "revision":
				bad.Settled[0].Data[32] = 0
			case "truncate":
				bad.Settled[0].Data = bad.Settled[0].Data[:10]
			case "coverage":
				bad.Settled[0].Last--
			case "overlap":
				bad.Records[0].Receipt.Sequence = 1
			case "v1-with-packed":
				bad.Version = 1
			}
			require.Error(t, restore(&bad))
		})
	}
	// Frozen prefixes survive append, growth and partial pruning at the source.
	frozen := slices.Clone(image.Settled[0].Data)
	request := bridgeRequest(scope, 1501, time.Now())
	require.Equal(t, "admitted", s.admitBridgePacket(request).Outcome)
	paceBridge(t, s)
	<-rig.wire.packets
	require.Equal(t, frozen, image.Settled[0].Data)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 700))
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	s.mu.Unlock()
	require.EqualValues(t, 701, s.agent.v2.bridge.settled[0].First)
	_, err = validateBridgeSettled(s.exportBridgeLocked(), s.agent.v2.bridge.state)
	require.NoError(t, err)
	require.Equal(t, frozen, image.Settled[0].Data)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1501))
	require.Empty(t, s.agent.v2.bridge.settled)
	require.Empty(t, s.agent.v2.bridge.records)
	require.Zero(t, s.agent.v2.bridge.memory.Load())
}

func TestBridgeOutputReadsUnpackedExactV1(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	retainBridgeStream(t, rig.s, scope, 1, 160, rig.wire)
	for seq := uint64(2); seq <= 3; seq++ {
		require.Equal(t, "admitted", rig.s.admitBridgePacket(bridgeRequest(scope, seq, time.Now())).Outcome)
	}
	image := rig.s.exportBridgeLocked()
	image.Version = 1
	target, _ := newBridgeRig(t, false)
	target.s.agent.latest = cloneAgentState(rig.s.state.Agent)
	target.s.state.Agent = cloneAgentState(rig.s.state.Agent)
	require.NoError(t, target.s.restoreBridgeOutput(&movePayload{Bridge: image}))
	require.NoError(t, target.s.restorePlayback(&movePayload{Bridge: image}))
}

func bridgePackedBytes(ranges []bridgeSettledRange) int {
	n := 0
	for _, r := range ranges {
		n += len(r.Data)
	}
	return n
}

func TestBridgeOutputPackedEncodingBound(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	retainBridgeStream(t, rig.s, scope, 1500, 1200, rig.wire)
	for _, r := range rig.s.agent.v2.bridge.settled {
		encoded, err := json.Marshal(r)
		require.NoError(t, err)
		require.Equal(t, bridgeSettledMemory(r)+bridgeSettledFrozenHeaderCharge+3*int64(len(encoded)+2), bridgeSettledCost(r))
		require.LessOrEqual(t, r.Last-r.First+1, uint64(bridgeSettledEntries))
		require.GreaterOrEqual(t, rig.s.bridgeEncodingTemporaryBytes(), 2*(bridgeBase64Bytes(int64(len(r.Data)))+2))
	}
	// Tightened observer bounds must still reserve the packed base64 fragment.
	b := rig.s.agent.v2.bridge
	b.limits.ObserverRequestBytes = 1
	b.limits.ObserverMailboxBytes = 1
	b.limits.AdapterMoveOnlyBytes = 1
	require.GreaterOrEqual(t, rig.s.bridgeEncodingTemporaryBytes(), 2*(bridgeBase64Bytes(bridgeSettledEntries*(32+10))+2))
}

func TestBridgeOutputPackedReceiptCeilingRejectsWrap(t *testing.T) {
	r := bridgeSettledRange{First: ^uint64(0), Last: ^uint64(0), Turn: 1, Stream: 1, Data: make([]byte, 33)}
	r.Data[32] = 1
	r.Chain = bridgeSettledChain(r.Scope, r.Seed, r.First, r.Turn, r.Stream, 1, [32]byte{})
	state := &bridgeOutputState{Through: ^uint64(0), Sent: ^uint64(0), Pruned: ^uint64(0) - 1, Revision: 1, LastStream: 1, LastTurn: 1}
	_, err := validateBridgeSettled(&bridgeOutputMove{Version: bridgeOutputMoveVersion, Settled: []bridgeSettledRange{r}}, state)
	require.Error(t, err, "the excluded sequence ceiling cannot wrap the digest scanner")
}
