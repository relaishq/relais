package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestBridgeOutputCreditPreservesExportReserve(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
			rig.app.state.History = make([]byte, 8192)
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, err)
			count := 0
			for seq := uint64(1); seq < 1000; seq++ {
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
				s.mu.Lock()
				live := s.agent.v2.bridge.memory.Load() + s.moveReserved
				cost := live + s.exportScratchBytes(live)
				s.mu.Unlock()
				require.LessOrEqual(t, cost, s.bridgeLookaheadBudget(), "credit must include actor cost and preserve export headroom")
			}
			require.Greater(t, count, 0)
			t.Logf("packet_bytes=%d packets=%d media=%s", size, count, time.Duration(count)*20*time.Millisecond)
		})
	}
}

func seedBridgeOverCut(t *testing.T) (acceptedEventRig, BridgeOutputScope) {
	t.Helper()
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
	rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
	rig.app.state.History = make([]byte, 8192)
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	b := s.agent.v2.bridge
	b.limits.ActorReserveBytes = 4096
	b.limits.ExportHeadroomBytes = 4096
	for seq := uint64(1); seq <= 22; seq++ {
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgePacket(1200)
		r.digest = sha256.Sum256(r.data)
		require.Equal(t, "admitted", s.admitBridgePacket(r).Outcome)
	}
	b.limits = BridgeOutputLimits{}.Defaults()
	// Simulate a tighter destination profile, independent of the current model.
	b.limits.ExportHeadroomBytes = (1 << 20) - int(s.bridgeExportCostLocked()) + 10000
	require.Greater(t, s.bridgeExportCostLocked(), s.bridgeLookaheadBudget())
	return rig, scope
}
func TestBridgeOutputDrainToFitBeforePause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := seedBridgeOverCut(t)
		s := rig.s
		request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(time.Second), moveOnly: make(chan agentMoveExport, 1)}
		done := make(chan bool, 1)
		go func() { done <- s.finishAgentFlush(request) }()
		synctest.Wait()
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		require.Zero(t, credit.PacketSlots)
		require.False(t, s.agent.paused, "waiting for playback must not pause the actor input")
		// Retire the old payloads while the actor waits, with no timer sleeps.
		for range 22 {
			paceBridge(t, s)
		}
		synctest.Wait()
		require.True(t, <-done)
		require.NoError(t, <-request.done)
		image := <-request.moveOnly
		data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge, Tasks: image.tasks, Audio: image.audio, Bytes: image.payload})
		require.NoError(t, err)
		snap, err := decodeSnapshot(data)
		require.NoError(t, err)
		target, _ := newBridgeRig(t, false)
		target.s.state.Agent = cloneAgentState(snap.State.Agent)
		target.s.agent.latest = cloneAgentState(snap.State.Agent)
		require.NoError(t, target.s.restoreBridgeOutput(snap.AgentMoveOnly))
		require.EqualValues(t, len(image.bridge.Queue), len(target.s.agent.v2.playback.bridgeQueue))
	})
}
func TestBridgeOutputDrainDeadlinePreservesSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := seedBridgeOverCut(t)
		s := rig.s
		before := s.agent.v2.bridge.state.Through
		request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(time.Second), moveOnly: make(chan agentMoveExport, 1)}
		done := make(chan bool, 1)
		go func() { done <- s.finishAgentFlush(request) }()
		synctest.Wait()
		require.False(t, s.agent.paused)
		time.Sleep(time.Second)
		require.True(t, <-done)
		require.ErrorIs(t, <-request.done, ErrExportNotFlushed)
		require.False(t, s.agent.paused)
		require.False(t, s.fenced.Load())
		require.NoError(t, s.ctx.Err())
		require.Equal(t, before, s.agent.v2.bridge.state.Through)
		for range 22 {
			paceBridge(t, s)
		}
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		require.Greater(t, credit.PacketSlots, 0)
		require.Equal(t, "admitted", submitBridgeAfterStarting(t, s, scope, before+1).Outcome)
	})
}
func submitBridgeAfterStarting(t *testing.T, s *session, scope BridgeOutputScope, seq uint64) BridgeOutputReceipt {
	t.Helper()
	startBridgeLoop(t, s)
	return submitBridge(t, s, scope, 1, seq)
}

// Exercise the production actor flush, exact transport capture, bridge restore
// and playback restore. Epoch reassignment remains the authenticated transport's
// next slice; this test preserves the same lineage and wire sequence.
func TestBridgeOutputBurstMovesAtExactCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		target, _ := newBridgeRig(t, false)
		source := rig.s
		source.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		target.s.worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		source.storeClockAt = time.Time{}
		target.s.storeClockAt = time.Time{}
		rig.app.state.Bytes = make([]byte, source.agent.v2.registration.Bounds.MaxStateBytes)
		rig.app.state.History = make([]byte, 8192)
		var traceMu sync.Mutex
		ids := map[uint64]bool{}
		var traceErr string
		var previous time.Time
		var maxGap time.Duration
		attach := func(r acceptedEventRig) {
			r.s.worker.cfg.PacketTrace = func(p PacketTrace) {
				if p.Protocol != "SRTP" {
					return
				}
				id := binary.BigEndian.Uint64(r.s.plainBuf[13:21])
				traceMu.Lock()
				defer traceMu.Unlock()
				if ids[id] {
					traceErr = fmt.Sprint("duplicate wire packet ", id)
				}
				ids[id] = true
				if !previous.IsZero() {
					maxGap = max(maxGap, p.SentAt.Sub(previous))
				}
				previous = p.SentAt
			}
			go func() {
				for {
					select {
					case <-r.s.ctx.Done():
						return
					case <-r.wire.packets:
					}
				}
			}()
		}
		attach(rig)
		startBridgeActor(t, source)
		synctest.Wait()
		current := source
		moves := 0
		for seq := uint64(1); seq <= 400; seq++ {
			time.Sleep(5 * time.Millisecond)
			for {
				credit, err := current.worker.BridgeOutputCredit(scope)
				require.NoError(t, err)
				if credit.PacketSlots > 0 && credit.PayloadBytes >= 160 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			packet := bridgePacket(160)
			binary.BigEndian.PutUint64(packet[1:9], seq)
			receipt, err := current.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, packet)
			require.NoError(t, err)
			require.Equal(t, "admitted", receipt.Outcome)
			// The unit store acknowledgement and explicit producer horizon permit
			// retirement, just as the measurement's real writer does.
			current.mu.Lock()
			sent := current.agent.v2.bridge.state.Sent
			current.bridgeCheckpointedLocked(cloneAgentState(current.state.Agent), seq)
			current.mu.Unlock()
			require.NoError(t, current.worker.AdvanceBridgeReplayHorizon(scope, sent))
			if seq == 200 {
				data, release, err := current.export()
				require.NoError(t, err)
				snap, err := decodeSnapshot(data)
				require.NoError(t, err)
				require.EqualValues(t, seq, snap.State.Agent.V2.Bridge.Through)
				dst := target.s
				dst.state = snap.State
				dst.agent.latest = cloneAgentState(snap.State.Agent)
				dst.agent.v2.instance = &acceptedEventAgent{state: v2State(snap.State.Agent)}
				require.NoError(t, dst.restoreBridgeOutput(snap.AgentMoveOnly))
				require.NoError(t, dst.restorePlayback(snap.AgentMoveOnly))
				require.True(t, playbackEqual(snap.State.Agent.V2.Playback, dst.playbackSnapshotLocked()))
				release()
				current.cancel()
				attach(target)
				startBridgeActor(t, dst)
				current = dst
				moves++
			}
		}
		require.NoError(t, current.worker.EndBridgeOutput(scope, 1, 400))
		time.Sleep(5 * time.Second)
		synctest.Wait()
		traceMu.Lock()
		count, problem, gap := len(ids), traceErr, maxGap
		traceMu.Unlock()
		require.Empty(t, problem)
		require.Equal(t, 400, count)
		require.Equal(t, 1, moves)
		t.Logf("burst packets=%d exact_moves=%d duplicate_ids=0 lost_ids=0 max_wire_gap=%s", count, moves, gap)
		require.LessOrEqual(t, gap, 20*time.Millisecond, "backlogged controlled-clock playback cannot starve during the move")
		require.Empty(t, current.agent.v2.playback.bridgeQueue)
		require.False(t, source.agent.failed.Load())
		require.False(t, current.agent.failed.Load())
	})
}

func TestBridgeOutputHeadroomProfileValidation(t *testing.T) {
	for _, limits := range []BridgeOutputLimits{{ObserverRequestBytes: -1}, {ObserverRequestBytes: 65536}, {ExportHeadroomBytes: -1}, {ExportHeadroomBytes: 1 << 20}, {ActorReserveBytes: 1<<20 - 4096, ExportHeadroomBytes: 4096}} {
		require.ErrorIs(t, limits.Validate(), agent.ErrCapacity)
	}
	defaults := BridgeOutputLimits{}.Defaults()
	require.Equal(t, (1<<20)/5, defaults.ExportHeadroomBytes)
	require.Equal(t, 4*time.Second, defaults.AudioDuration)
}
func TestBridgeOutputCreditBatchFits(t *testing.T) {
	for _, size := range []int{160, 1200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig, scope := newBridgeRig(t, false)
				s := rig.s
				credit, err := s.worker.BridgeOutputCredit(scope)
				require.NoError(t, err)
				count := min(credit.PacketSlots, credit.PayloadBytes/size)
				require.Positive(t, count)
				replies := make(chan BridgeOutputReceipt, count)
				for n := 1; n <= count; n++ {
					seq := uint64(n)
					go func() {
						r, e := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, bridgePacket(size))
						if e != nil {
							t.Error(e)
						}
						replies <- r
					}()
					// Queue in sequence without advancing the admission clock.
					synctest.Wait()
					require.Len(t, s.agent.v2.bridge.ingress, n)
				}
				startBridgeLoop(t, s)
				for range count {
					receipt := <-replies
					require.Equal(t, "admitted", receipt.Outcome, "%+v", receipt)
				}
				s.mu.Lock()
				cost, budget := s.bridgeExportCostLocked(), s.bridgeLookaheadBudget()
				s.mu.Unlock()
				require.LessOrEqual(t, cost, budget)
			})
		})
	}
}

func TestBridgeOutputWorkerDrainKeepsUnmovedSessionsLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sessions []*session
		var scopes []BridgeOutputScope
		var worker *Worker
		for i := 0; i < 3; i++ {
			rig, scope := newBridgeRig(t, false)
			s := rig.s
			if worker == nil {
				worker = s.worker
				worker.sessions = map[string]*session{}
			}
			s.worker = worker
			s.id = fmt.Sprintf("drain-bridge-%d", i)
			s.state.ID = s.id
			s.state.ICE.LocalUfrag = s.id
			scope.SessionID = s.id
			lease, err := rig.store.Claim(context.Background(), s.id, worker.localAddr, time.Hour)
			require.NoError(t, err)
			if i > 0 {
				lease, err = worker.cfg.Relay.Owners.Claim(context.Background(), s.id, worker.localAddr, time.Hour)
				require.NoError(t, err)
			}
			s.lease = lease
			scope.Epoch = lease.Epoch
			worker.sessions[s.id] = s
			s.agent.v2.bridge = newBridgeOutputHost(worker.cfg.Agent.BridgeOutput)
			s.agent.v2.playback.bridge = s.agent.v2.bridge
			require.NoError(t, worker.BindBridgeOutput(scope))
			_, err = worker.BeginBridgeOutput(scope, 1)
			require.NoError(t, err)
			maximumBridgeObservers(t, rig)
			require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
			s.mu.Lock()
			s.agent.v2.playback.initialized = true
			s.agent.v2.playback.nextTS = s.state.Audio.InitialTS
			s.agent.v2.playback.nextAt = s.taskClockLocked().Add(time.Hour)
			s.mu.Unlock()
			sessions = append(sessions, s)
			scopes = append(scopes, scope)
		}
		worker.cfg.Agent.CallbackTimeout = 10 * time.Millisecond
		worker.BeginDrain()
		for _, s := range sessions {
			startBridgeActor(t, s)
		}
		t.Cleanup(func() {
			for _, s := range sessions {
				s.cancel()
			}
		})
		synctest.Wait()
		for i, s := range sessions {
			// Control has not reached these sessions. Their next packets keep arriving.
			for j := i; j < len(sessions); j++ {
				credit, err := worker.BridgeOutputCredit(scopes[j])
				require.NoError(t, err)
				require.Positive(t, credit.PacketSlots)
				seq := uint64(i + 2)
				r, err := worker.SubmitBridgeOutput(context.Background(), scopes[j], 1, seq, bridgePacket(160))
				require.NoError(t, err)
				require.Equal(t, "admitted", r.Outcome)
			}
			data, release, err := s.export()
			require.NoError(t, err)
			release()
			snap, err := decodeSnapshot(data)
			require.NoError(t, err)
			require.EqualValues(t, i+2, snap.State.Agent.V2.Bridge.Through)
			require.Len(t, snap.AgentMoveOnly.Bridge.Queue, i+2)
			require.False(t, s.agent.failed.Load())
		}
	})
}
