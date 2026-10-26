package mediaworker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func fillBridgeToCap(t *testing.T, s *session, scope BridgeOutputScope, size int) int {
	t.Helper()
	count := 0
	for seq := uint64(1); seq < 3000; seq++ {
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		if credit.PacketSlots == 0 || credit.PayloadBytes < size {
			break
		}
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgePacket(size)
		r.digest = sha256.Sum256(r.data)
		require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
		count++
	}
	return count
}
func bridgeExportReservationFits(s *session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.reserveV2ExportScratch()
	if err == nil {
		release, _ := s.claimV2ExportScratch()
		release()
	}
	return err
}
func TestBridgeOutputCapWithObserverWork(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			maximumBridgeObservers(t, rig)
			s := rig.s
			require.NoError(t, bridgeExportReservationFits(s), "empty queue at maximum allowed observer requests must fit")
			count := fillBridgeToCap(t, s, scope, size)
			require.Positive(t, count)
			s.mu.Lock()
			require.NoError(t, s.reserveV2ExportScratch())
			held := s.exportScratch.bytes.Load()
			cost := s.bridgeExportCostLocked()
			require.Equal(t, cost, s.moveReserved+s.agent.v2.bridge.memory.Load()+held, "credit and export share one model")
			require.LessOrEqual(t, cost, s.bridgeLookaheadBudget())
			s.agent.paused = true
			s.mu.Unlock()
			image, err := s.saveV2MoveOnly(context.Background())
			require.NoError(t, err)
			encoded, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio, Bytes: image.payload})
			require.NoError(t, err)
			require.NoError(t, validateV2Wire(encoded))
			require.GreaterOrEqual(t, held, 3*int64(len(encoded)))
			require.LessOrEqual(t, cost+int64(s.agent.v2.bridge.limits.ExportHeadroomBytes), int64(1<<20))
			t.Logf("packet_bytes=%d requests=%d owned_actor=%d packets=%d media=%s cost=%d scratch=%d encoded=%d", size, s.moveRequestBytes.Load(), s.moveReserved, count, time.Duration(count)*20*time.Millisecond, cost, held, len(encoded))
		})
	}
}

func TestBridgeOutputMaximumObserversMoveAtCap(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig, scope := newBridgeRig(t, false)
				maximumBridgeObservers(t, rig)
				s := rig.s
				s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
				count := fillBridgeToCap(t, s, scope, size)
				require.Positive(t, count)
				// Park only playback: the production actor can still finish the move.
				s.mu.Lock()
				s.agent.v2.playback.initialized = true
				s.agent.v2.playback.nextTS = s.state.Audio.InitialTS
				s.agent.v2.playback.nextAt = s.taskClockLocked().Add(time.Hour)
				s.mu.Unlock()
				startBridgeActor(t, s)
				synctest.Wait()
				data, release, err := s.export()
				require.NoError(t, err)
				defer release()
				cut, err := decodeSnapshot(data)
				require.NoError(t, err)
				require.Len(t, cut.AgentMoveOnly.Bridge.Queue, count)
				require.Len(t, cut.AgentMoveOnly.Tasks, 2)
				require.Equal(t, uint64(count), cut.State.Agent.V2.Bridge.Through)
				require.False(t, s.agent.failed.Load())
				t.Logf("planned export packet_bytes=%d queued=%d wire_bytes=%d", size, count, len(data))
			})
		})
	}
}

// Later in-bounds actor work must fit without relying on receipt pruning or playback.
func TestBridgeOutputActorWorkAfterCreditCut(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			s.agent.v2.bridge.limits.ObserverRequestBytes = 32 << 10
			s.agent.v2.bridge.limits.ObserverMailboxBytes = 8 << 10
			count := fillBridgeToCap(t, s, scope, size)
			require.Positive(t, count)
			maximumBridgeObservers(t, rig) // two 16 KiB requests, after filling without observers
			require.NoError(t, bridgeExportReservationFits(s), "in-bounds growth after credit must still fit without draining")
			t.Logf("packet_bytes=%d packets=%d cost=%d budget=%d", size, count, s.bridgeExportCostLocked(), s.bridgeLookaheadBudget())
		})
	}
}
