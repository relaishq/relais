package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"

	"github.com/stretchr/testify/require"
)

func TestBridgeOutputExportCapacity(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
			rig.app.state.History = make([]byte, 8192)
			_, initialErr := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, initialErr)
			var count int
			for seq := uint64(1); seq <= 2000; seq++ {
				request := bridgeRequest(scope, seq, time.Now())
				request.data = bridgePacket(size)
				request.digest = sha256.Sum256(request.data)
				r := s.admitBridgePacket(request)
				if r.Outcome != "admitted" {
					break
				}
				s.mu.Lock()
				err := s.reserveV2ExportScratch()
				if err == nil {
					release, _ := s.claimV2ExportScratch()
					release()
				}
				s.mu.Unlock()
				if err != nil {
					break
				}
				count++
			}
			require.Greater(t, count, 0)
			t.Logf("packet_bytes=%d exportable_packets=%d payload_bytes=%d media=%s", size, count, size*count, time.Duration(count)*20*time.Millisecond)
			// Drive the real exact encoding at the supported cut, including working,
			// history, transport and the scoped digest/receipt ledger.
			fit, sc := newBridgeRig(t, false)
			ss := fit.s
			fit.app.state.Bytes = make([]byte, ss.agent.v2.registration.Bounds.MaxStateBytes)
			fit.app.state.History = make([]byte, 8192)
			_, err := (acceptedEvents{ss}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, err)
			for seq := 1; seq <= count; seq++ {
				request := bridgeRequest(sc, uint64(seq), time.Now())
				request.data = bridgePacket(size)
				request.digest = sha256.Sum256(request.data)
				require.Equal(t, "admitted", ss.admitBridgePacket(request).Outcome)
			}
			ss.mu.Lock()
			scratchErr := ss.reserveV2ExportScratch()
			if scratchErr != nil {
				ss.mu.Unlock()
				require.NoError(t, scratchErr)
			}
			held := ss.exportScratch.bytes.Load()
			live := ss.agent.v2.bridge.memory.Load() + ss.moveReserved
			ss.agent.paused = true
			ss.mu.Unlock()
			image, err := ss.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			encoded, err := ss.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio})
			require.NoError(t, err)
			require.NoError(t, validateV2Wire(encoded))
			require.LessOrEqual(t, live+held+int64(ss.agent.v2.bridge.limits.ActorReserveBytes+ss.agent.v2.bridge.limits.ExportHeadroomBytes), int64(1<<20))
			require.GreaterOrEqual(t, held, 3*int64(len(encoded)), "reserve growing JSON encoder buffer and returned bytes")
			t.Logf("encoded=%d live=%d scratch=%d total=%d", len(encoded), live, held, live+held)
		})
	}
}

func TestBridgeOutputEncodingChargesActualJSON(t *testing.T) {
	for _, size := range []int{0, 1, 160, 1200} {
		r := bridgePacketRecord{Scope: BridgeOutputScope{SessionID: "session", BotInstance: "bot", Epoch: ^uint64(0), Generation: ^uint64(0), LineageID: "lineage"}, Digest: sha256.Sum256([]byte("audio")), Duration: 20 * time.Millisecond, Length: size, Stream: ^uint64(0)}
		r.Receipt = BridgeOutputReceipt{BridgeOutputScope: r.Scope, Sequence: ^uint64(0), Turn: ^agent.Turn(0), Revision: ^uint64(0), Outcome: "refused", Reason: "backpressure", Consumed: true}
		r.Payload = make([]byte, size)
		encoded, err := json.Marshal(r)
		require.NoError(t, err)
		require.Equal(t, len(encoded), bridgeRecordJSONBytes(r))
	}
	for _, size := range []int{1, 3, 160, 3000000, 3200000} {
		data := make([]byte, size)
		require.Equal(t, validateV2Wire(data) == nil, validateExactWire(data, true) == nil)
	}
}

// A writer can acknowledge after SaveMoveOnly freezes the bridge image, before
// the exact snapshot captures the actor pair. Both halves must keep one cut.
func TestBridgeOutputCheckpointDuringExportPreservesCut(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint("empty=", empty), func(t *testing.T) {
			rig, scope := newBridgeRig(t, true)
			s := rig.s
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, err)
			if empty {
				require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 0))
				require.NoError(t, s.deliverClockEvent())
			} else {
				submitBridge(t, s, scope, 1, 1)
				paceBridge(t, s)
				submitBridge(t, s, scope, 1, 2)
			}
			through := s.agent.v2.bridge.state.Sent
			require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, through))
			ack := cloneAgentState(s.state.Agent)
			s.publishAgent()
			s.mu.Lock()
			err = s.reserveV2ExportScratch()
			s.agent.paused = true
			s.mu.Unlock()
			require.NoError(t, err)
			image, err := s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			s.mu.Lock()
			s.bridgeCheckpointedLocked(ack, 7)
			s.mu.Unlock()
			data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bytes: image.payload, Tasks: image.tasks, Audio: image.audio, Bridge: image.bridge})
			require.NoError(t, err)
			snap, err := decodeSnapshot(data)
			require.NoError(t, err)
			target, _ := newBridgeRig(t, false)
			dst := target.s
			dst.agent.latest, dst.state.Agent = cloneAgentState(snap.State.Agent), cloneAgentState(snap.State.Agent)
			dst.agent.v2.bridge = newBridgeOutputHost(BridgeOutputLimits{})
			dst.agent.v2.playback = playbackRuntime{bridge: dst.agent.v2.bridge}
			require.NoError(t, dst.restoreBridgeOutput(snap.AgentMoveOnly), "frozen image and actor snapshot must match")
			// Rollback releases the cut. An unchanged acknowledgement must still
			// compact the deferred records and empty turn on its next delivery.
			s.mu.Lock()
			s.agent.paused = false
			s.fenced.Store(false)
			s.bridgeCheckpointedLocked(ack, 7)
			records, turns := len(s.agent.v2.bridge.records), len(s.agent.v2.bridge.state.Turns)
			s.mu.Unlock()
			if empty {
				require.Zero(t, turns)
			} else {
				require.Equal(t, 1, records)
			}
		})
	}
}

func TestBridgeOutputCheckpointWhileFencedDefersCompaction(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	paceBridge(t, s)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1))
	s.mu.Lock()
	s.fenced.Store(true)
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	held := len(s.agent.v2.bridge.records)
	s.fenced.Store(false)
	s.mu.Unlock()
	require.Equal(t, 1, held)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1))
	require.Empty(t, s.agent.v2.bridge.records)
}
