package mediaworker

import (
	"context"
	"crypto/sha256"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Exercise the public credit endpoint while the production pacer wakes before
// its deadline. No output, scope, hold or budget can change in this fixture.
func TestBridgeOutputCreditSurvivesPacerWakes(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
	s.mu.Lock()
	p := &s.agent.v2.playback
	p.initialized = true
	p.nextAt = s.taskClockLocked().Add(time.Hour)
	s.mu.Unlock()
	const polls = 4000
	var zero, wakes atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := s.paceOne()
			if err != nil {
				t.Error(err)
				return
			}
			wakes.Add(1)
			runtime.Gosched()
		}
	}()
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range polls / 4 {
				credit, err := s.worker.BridgeOutputCredit(scope)
				if err != nil {
					t.Error(err)
					return
				}
				if credit.PacketSlots == 0 || credit.PayloadBytes < 160 {
					zero.Add(1)
				}
			}
		})
	}
	group.Wait()
	close(stop)
	<-done
	require.Positive(t, wakes.Load())
	t.Logf("polls=%d pacer_wakes=%d spurious_zero=%d cost=%d budget=%d", polls, wakes.Load(), zero.Load(), s.bridgeExportCostLocked(), s.bridgeLookaheadBudget())
	require.Zero(t, zero.Load(), "pacer wakes cannot withdraw available credit")
}

// With no contention, the complete public call bounds the cost of its critical
// section. Report timings rather than imposing a microsecond CI deadline.
func TestBridgeOutputCreditCriticalSectionCost(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	for seq := uint64(1); seq <= 5; seq++ {
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, seq, time.Now())).Outcome)
	}
	measureBridgeCreditCallCost(t, s, scope)
}

func measureBridgeCreditCallCost(t *testing.T, s *session, scope BridgeOutputScope) {
	t.Helper()
	s.mu.Lock()
	metadata, err := bridgeCreditMetadataCharge(s.agent.v2.bridge.state, s.playbackSnapshotLocked(), s.agent.v2.bridge.limits.PendingPackets)
	s.mu.Unlock()
	require.NoError(t, err)
	var samples []time.Duration
	for range 1000 {
		at := time.Now()
		credit, err := s.worker.BridgeOutputCredit(scope)
		samples = append(samples, time.Since(at))
		require.NoError(t, err)
		require.Positive(t, credit.PacketSlots)
	}
	slices.Sort(samples)
	t.Logf("uncontended_credit_call metadata_json_bytes=%d samples=%d p50=%s p95=%s max=%s", metadata, len(samples), samples[500], samples[950], samples[999])
}

// Repeated early wakes must preserve the immutable cut observed by concurrent
// admissions and checkpoint capture, rather than publishing equivalent clones.
func TestBridgeOutputPacerWakeKeepsUnchangedCut(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
	s.mu.Lock()
	p := &s.agent.v2.playback
	p.initialized = true
	p.nextAt = s.taskClockLocked().Add(time.Hour)
	cut := s.agent.v2.bridge.state
	s.mu.Unlock()
	for range 10 {
		_, err := s.paceOne()
		require.NoError(t, err)
		require.Same(t, cut, s.agent.v2.bridge.state, "an early wake changed no bridge state")
	}
}

// Recovery adds a pruned floor to metadata while five maximum-size packets
// remain queued. Credit must preserve their 100 ms backlog at maximum tokens.
func TestBridgeOutputCreditAfterPruneKeepsQueuedHeadroom(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	oldID := s.id
	s.id = strings.Repeat("s", 64)
	s.state.ID, s.state.ICE.LocalUfrag = s.id, s.id
	delete(s.worker.sessions, oldID)
	s.worker.sessions[s.id] = s
	scope.SessionID, scope.BotInstance, scope.LineageID = s.id, strings.Repeat("b", 64), strings.Repeat("l", 64)
	s.agent.v2.bridge.state = nil
	require.NoError(t, s.worker.BindBridgeOutput(scope))
	_, err := s.worker.BeginBridgeOutput(scope, 1)
	require.NoError(t, err)
	retainBridgeStream(t, s, scope, 1499, 1200, rig.wire)
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 3)
	s.mu.Unlock()
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1498))
	for seq := uint64(1500); seq <= 1504; seq++ {
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgePacket(1200)
		r.digest = sha256.Sum256(r.data)
		require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
	}
	// A genuine late wake leaves active RTP rebase coverage in metadata.
	s.mu.Lock()
	s.agent.v2.playback.nextAt = s.taskClockLocked().Add(-100 * time.Millisecond)
	s.mu.Unlock()
	_, err = s.paceOne()
	require.NoError(t, err)
	<-rig.wire.packets
	r := bridgeRequest(scope, 1505, time.Now())
	r.data = bridgePacket(1200)
	r.digest = sha256.Sum256(r.data)
	require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
	s.mu.Lock()
	require.Positive(t, s.agent.v2.bridge.state.RebaseTicks)
	s.agent.v2.playback.nextAt = time.Date(2026, 10, 25, 0, 0, 0, 123456789, time.FixedZone("offset", -7*60*60))
	s.agent.v2.playback.nextTS = ^uint32(0)
	cost := s.bridgeExportCostLocked()
	s.mu.Unlock()
	credit, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	t.Logf("pruned=1498 queued=5 cost=%d budget=%d slots=%d bytes=%d", cost, s.bridgeLookaheadBudget(), credit.PacketSlots, credit.PayloadBytes)
	require.Positive(t, credit.PacketSlots)
	require.GreaterOrEqual(t, credit.PayloadBytes, 1200)
	measureBridgeCreditCallCost(t, s, scope)
	// The protocol's real ceiling is Through, not MaxUint64. Its maximum valid
	// end plus maximum interrupt/clear floors must still fit reserved metadata.
	require.ErrorIs(t, s.worker.EndBridgeOutput(scope, 1, 1506), ErrBridgeOutputOrder)
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 1505))
	startBridgeLoop(t, s)
	_, err = s.worker.ClearBridgeAudio(context.Background(), scope, "keep-current", 1)
	require.NoError(t, err)
	_, err = s.worker.InterruptBridgeOutput(context.Background(), scope, "max-cancel", ^agent.Turn(0), false)
	require.NoError(t, err)
	for _, clear := range []bool{false, true} {
		if clear {
			_, err = s.worker.ClearBridgeAudio(context.Background(), scope, "max-clear", ^agent.Turn(0))
			require.NoError(t, err)
		}
		s.mu.Lock()
		charge, err := bridgeMetadataCharge(s.agent.v2.bridge.state, s.playbackSnapshotLocked())
		queued := len(s.agent.v2.playback.bridgeQueue)
		s.mu.Unlock()
		require.NoError(t, err)
		require.LessOrEqual(t, charge+bridgeTerminalReserve, bridgeMetadataPartition)
		if clear {
			require.Zero(t, queued)
		} else {
			require.Equal(t, 5, queued)
		}
	}
}

// Consuming a queued gap is real playback progress even when its refusal was
// already in the terminal floor. It must still invalidate an old cut.
func TestBridgeOutputPacerPublishesGapProgress(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	r := bridgeRequest(scope, 1, time.Now())
	r.invalid = true
	require.Equal(t, "refused", s.admitBridgePacket(r).Outcome)
	cut := s.agent.v2.bridge.state
	_, err := s.paceOne()
	require.NoError(t, err)
	require.NotSame(t, cut, s.agent.v2.bridge.state)
	require.Greater(t, s.agent.v2.bridge.state.Revision, cut.Revision)
	require.Empty(t, s.agent.v2.playback.bridgeQueue)
}
