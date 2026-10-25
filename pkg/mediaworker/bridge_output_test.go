package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func newBridgeRig(t *testing.T, start bool) (acceptedEventRig, BridgeOutputScope) {
	t.Helper()
	rig := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	s := rig.s
	s.state.Agent = agentState{}
	s.componentMode = true
	s.worker.cfg.Agent.BridgeOutput = BridgeOutputLimits{}.Defaults()
	s.worker.cfg.Agent.CallbackTimeout = time.Hour
	s.worker.cfg.Agent.QueueCapacity = 32
	s.worker.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "bridge", Factory: func(agent.FactoryArgs) agent.Agent { return rig.app }})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	scope := BridgeOutputScope{SessionID: s.id, BotInstance: "bot", Epoch: 1, Generation: 1, LineageID: "lineage"}
	require.NoError(t, s.worker.BindBridgeOutput(scope))
	_, err := s.worker.BeginBridgeOutput(scope, 1)
	require.NoError(t, err)
	if start {
		startBridgeLoop(t, s)
	}
	return rig, scope
}
func startBridgeLoop(t *testing.T, s *session) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); s.bridgeOutputLoop() }()
	t.Cleanup(func() { s.cancel(); <-done })
}
func bridgePacket(n int) []byte { p := make([]byte, n); p[0] = 0xf8; return p }
func submitBridge(t *testing.T, s *session, scope BridgeOutputScope, turn agent.Turn, seq uint64) BridgeOutputReceipt {
	t.Helper()
	r, err := s.worker.SubmitBridgeOutput(context.Background(), scope, turn, seq, bridgePacket(160))
	require.NoError(t, err)
	return r
}
func paceBridge(t *testing.T, s *session) {
	t.Helper()
	s.mu.Lock()
	s.agent.v2.playback.nextAt = s.taskClockLocked()
	s.mu.Unlock()
	_, err := s.paceOne()
	require.NoError(t, err)
}
func bridgeRequest(scope BridgeOutputScope, seq uint64, at time.Time) bridgeOutputRequest {
	data := bridgePacket(160)
	return bridgeOutputRequest{scope: scope, turn: 1, seq: seq, ingress: at, data: data, digest: sha256.Sum256(data), duration: 20 * time.Millisecond, done: make(chan BridgeOutputReceipt, 1)}
}
func TestBridgeOutputLatePacketContinuesTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		first := s.admitBridgePacket(bridgeRequest(scope, 1, time.Now().Add(-51*time.Millisecond)))
		require.Equal(t, "refused", first.Outcome)
		require.Equal(t, "output_late", first.Reason)
		next := s.admitBridgePacket(bridgeRequest(scope, 2, time.Now()))
		require.Equal(t, "admitted", next.Outcome)
		require.NoError(t, s.ctx.Err())
		require.False(t, s.agent.failed.Load())
		require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 2))
		paceBridge(t, s)
		require.NoError(t, s.deliverClockEvent())
		outcome, err := s.worker.BridgePlaybackOutcome(scope, 1)
		require.NoError(t, err)
		require.Equal(t, BridgePlaybackFinished{Turn: 1, Outcome: "gapped", GapCount: 1}, outcome)
		require.EqualValues(t, 2, s.agent.v2.bridge.state.Through)
	})
}
func TestBridgeOutputAdmissionIndependentOfParkedActor(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	entered, release := make(chan struct{}), make(chan struct{})
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		close(entered)
		<-release
		return agent.Effects{}, nil
	}
	actor := make(chan error, 1)
	go func() {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		actor <- err
	}()
	<-entered
	// The controlled callback budget is one hour; no real 100 ms race deadline.
	r := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, "admitted", r.Outcome)
	close(release)
	require.NoError(t, <-actor)
	require.NoError(t, s.ctx.Err())
	require.False(t, s.agent.failed.Load())
	require.EqualValues(t, 1, s.state.Agent.V2.Bridge.Through, "actor publication must preserve the host commit")
}
func TestBridgeOutputBlockedAdmissionAfterFastSave(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	saved := make(chan struct{}, 1)
	rig.app.save = func(context.Context) error { s.checkpoint.snapshotBudgetMu.Lock(); saved <- struct{}{}; return nil }
	actor := make(chan error, 1)
	go func() {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		actor <- err
	}()
	<-saved
	// The request waited in host ingress after Save. Supply its expired ingress
	// instant directly: no scheduler-speed or callback-deadline assumption.
	r := s.admitBridgePacket(bridgeRequest(scope, 1, time.Now().Add(-s.agent.v2.bridge.limits.AdmissionTimeout-time.Nanosecond)))
	require.Equal(t, "output_late", r.Reason)
	s.checkpoint.snapshotBudgetMu.Unlock()
	require.NoError(t, <-actor)
	rig.app.save = nil
	startBridgeLoop(t, s)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 2).Outcome)
	require.False(t, s.agent.failed.Load())
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputCreditBoundsAndPriority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		b := s.agent.v2.bridge
		replies := make(chan BridgeOutputReceipt, b.limits.PendingPackets)
		for i := 1; i <= b.limits.PendingPackets; i++ {
			seq := uint64(i)
			go func() {
				r, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, seq, bridgePacket(160))
				if err != nil {
					panic(err)
				}
				replies <- r
			}()
			synctest.Wait()
		}
		require.EqualValues(t, 32, b.reservedPackets.Load())
		_, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 33, bridgePacket(160))
		require.ErrorIs(t, err, agent.ErrBackpressure)
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		require.Zero(t, credit.PacketSlots)
		interrupted := make(chan BridgeInterruption, 1)
		go func() {
			r, err := s.worker.InterruptBridgeOutput(context.Background(), scope, "interrupt", 2, true)
			if err != nil {
				panic(err)
			}
			interrupted <- r
		}()
		synctest.Wait()
		startBridgeLoop(t, s)
		receipt := <-interrupted
		require.EqualValues(t, 2, receipt.CancelFloor)
		require.EqualValues(t, 2, receipt.ClearFloor)
		for range b.limits.PendingPackets {
			r := <-replies
			require.Equal(t, "canceled", r.Outcome)
		}
		require.LessOrEqual(t, b.memory.Load(), int64(32*bridgeRecordCharge))
		require.NoError(t, s.ctx.Err())
	})
}
func TestBridgeOutputDuplicateConflictLostReceiptAndRetirement(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	first := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, first, submitBridge(t, s, scope, 1, 1))
	changed := bridgePacket(160)
	changed[1] = 42
	conflict, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 1, changed)
	require.NoError(t, err)
	require.Equal(t, "conflict", conflict.Reason)
	// Ignore the original reply and reconcile through the production status API.
	status, err := s.worker.BridgeOutputStatus(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "settled", status.Status)
	require.Equal(t, first, *status.Receipt)
	paceBridge(t, s)
	status, err = s.worker.BridgeOutputStatus(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "sent", status.Receipt.Outcome)
	b := s.agent.v2.bridge
	require.Nil(t, b.records[1].Payload)
	require.Equal(t, sha256.Sum256(bridgePacket(160)), b.records[1].Digest)
	require.Zero(t, b.audioBytes)
	require.Zero(t, b.audioDuration)
	require.EqualValues(t, bridgeRecordCharge, b.memory.Load())
	_, err = s.worker.BridgeOutputCheckpointed(scope)
	require.ErrorIs(t, err, agent.ErrBackpressure, "local retirement cannot fabricate durability")
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1))
	require.Contains(t, b.records, uint64(1), "horizon alone cannot prune")
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 7)
	s.pruneBridgeLocked()
	s.mu.Unlock()
	require.Empty(t, b.records)
	require.Zero(t, b.memory.Load())
	settled := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, "settled", settled.Reason)
	require.Empty(t, s.agent.v2.playback.bridgeQueue)
	checkpointed, err := s.worker.BridgeOutputCheckpointed(scope)
	require.NoError(t, err)
	require.Contains(t, string(checkpointed), `"checkpoint_seq":7`)
}
func TestBridgeOutputStallsCoalesceRangesAndGapClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		s := rig.s
		p := &s.agent.v2.playback
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
		paceBridge(t, s)
		baseTS, baseAt := p.nextTS, p.nextAt
		for seq := uint64(2); seq <= 5; seq++ {
			require.Equal(t, "output_late", s.admitBridgePacket(bridgeRequest(scope, seq, time.Now().Add(-time.Second))).Reason)
		}
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, 6, time.Now())).Outcome)
		for seq := uint64(7); seq <= 10; seq++ {
			require.Equal(t, "output_late", s.admitBridgePacket(bridgeRequest(scope, seq, time.Now().Add(-time.Second))).Reason)
		}
		require.Len(t, s.agent.v2.bridge.state.Exceptions, 2)
		encoded, err := json.Marshal(s.agent.v2.bridge.state.Exceptions)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), s.agent.v2.bridge.limits.ReceiptBytes)
		s.storeClock = baseAt
		s.storeClockAt = time.Now()
		_, err = s.paceOne()
		require.NoError(t, err)
		require.Equal(t, baseTS+5*960, p.nextTS, "four gaps and one packet advance media time")
		require.Equal(t, baseAt.Add(20*time.Millisecond), p.nextAt, "gaps add no wall schedule delay")
		// A late wake already advanced the timestamp over the next gap interval.
		beforeTS := p.nextTS
		s.storeClock = p.nextAt.Add(100 * time.Millisecond)
		s.storeClockAt = time.Now()
		_, err = s.paceOne()
		require.NoError(t, err)
		require.Equal(t, beforeTS+4800, p.nextTS, "rebase-covered gaps cannot charge RTP time twice")
		require.Empty(t, p.bridgeQueue)
		// Coverage left beyond the captured sequence ceiling cannot be borrowed
		// by a later gap when the next late wake extends that ceiling.
		beforeTS = p.nextTS
		later := bridgeRequest(scope, 11, time.Now().Add(-time.Second))
		later.data = []byte{0x18, 0}
		later.digest = sha256.Sum256(later.data)
		later.duration = 60 * time.Millisecond
		require.Equal(t, "output_late", s.admitBridgePacket(later).Reason)
		s.storeClock = p.nextAt.Add(20 * time.Millisecond)
		s.storeClockAt = time.Now()
		_, err = s.paceOne()
		require.NoError(t, err)
		require.Equal(t, beforeTS+2880, p.nextTS, "a later gap cannot reuse old rebase coverage")
		require.NoError(t, s.ctx.Err())
	})
}
func TestBridgeOutputExactRestore(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 1).Outcome)
	paceBridge(t, s)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 2).Outcome)
	s.admitBridgePacket(bridgeRequest(scope, 3, time.Now().Add(-time.Second)))
	s.mu.Lock()
	saved := cloneAgentState(s.state.Agent)
	image := s.exportBridgeLocked()
	s.mu.Unlock()
	data, err := json.Marshal(image)
	require.NoError(t, err)
	var exact bridgeOutputMove
	require.NoError(t, json.Unmarshal(data, &exact))
	target, _ := newBridgeRig(t, false)
	dst := target.s
	dst.agent.latest = cloneAgentState(saved)
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.v2.bridge = newBridgeOutputHost(BridgeOutputLimits{})
	dst.agent.v2.playback = playbackRuntime{bridge: dst.agent.v2.bridge}
	require.NoError(t, dst.restoreBridgeOutput(&movePayload{Bridge: &exact}))
	require.NoError(t, dst.restorePlayback(&movePayload{Bridge: &exact}))
	require.True(t, bridgeStateEqual(saved.V2.Bridge, dst.agent.v2.bridge.state))
	require.Equal(t, image.Records, dst.exportBridgeLocked().Records)
	require.Equal(t, image.Queue, dst.agent.v2.playback.bridgeQueue)
	require.Nil(t, dst.agent.v2.bridge.records[1].Payload, "sent payload stays retired across an exact move")
	for _, mutation := range []string{"missing", "missing-receipt", "digest", "turn", "reference", "version"} {
		t.Run(mutation, func(t *testing.T) {
			var bad bridgeOutputMove
			require.NoError(t, json.Unmarshal(data, &bad))
			switch mutation {
			case "missing":
				bad.Records[1].Payload = nil
			case "missing-receipt":
				bad.Records = bad.Records[1:]
			case "digest":
				bad.Records[1].Payload[1]++
			case "turn":
				bad.Queue[0].Turn++
			case "reference":
				bad.Queue[0].Sequence = 99
			case "version":
				bad.Version++
			}
			dst.agent.v2.bridge = newBridgeOutputHost(BridgeOutputLimits{})
			dst.agent.v2.playback.bridge = dst.agent.v2.bridge
			require.Error(t, dst.restoreBridgeOutput(&movePayload{Bridge: &bad}))
		})
	}
	// Crash restore reports uncertainty and never reconstructs payloads.
	dst.agent.v2.bridge = newBridgeOutputHost(BridgeOutputLimits{})
	dst.agent.v2.playback = playbackRuntime{bridge: dst.agent.v2.bridge}
	require.NoError(t, dst.restoreBridgeOutput(nil))
	require.NoError(t, dst.restorePlayback(nil))
	require.Empty(t, dst.agent.v2.bridge.records)
	require.Empty(t, dst.agent.v2.playback.bridgeQueue)
	require.Equal(t, "unknown", dst.agent.v2.bridge.state.Turns[0].Outcome)
}
func TestBridgeOutputScopeAndProfile(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	stale := scope
	stale.Generation++
	r := submitBridge(t, s, stale, 1, 1)
	require.Equal(t, "stale_scope", r.Reason)
	require.Zero(t, s.agent.v2.bridge.state.Through)
	for _, l := range []BridgeOutputLimits{{PendingPackets: -1}, {PendingPackets: 33}, {PendingPayloadBytes: 38401}, {ReceiptRanges: 9}, {ReceiptBytes: 513}, {AdmissionTimeout: -1}, {AudioBytes: 2 << 20}} {
		require.Error(t, l.Validate(), fmt.Sprint(l))
	}
	require.NoError(t, (BridgeOutputLimits{}).Validate())
	plain := newAcceptedEventRig(t, agenthost.Bounds{}, nil)
	require.ErrorIs(t, plain.s.worker.BindBridgeOutput(BridgeOutputScope{SessionID: plain.s.id, BotInstance: "bot", Epoch: 1, Generation: 1, LineageID: "lineage"}), ErrBridgeOutputScope)
	before := cloneAgentState(plain.s.state.Agent)
	_, err := (acceptedEvents{plain.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	require.Nil(t, plain.s.state.Agent.V2.Bridge)
	require.Nil(t, before.V2.Bridge)
}
func TestBridgeOutputInterruptionSurvivesActorCommitAndRollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			rig, scope := newBridgeRig(t, true)
			s := rig.s
			submitBridge(t, s, scope, 1, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
				close(entered)
				<-release
				if rollback {
					return agent.Effects{}, fmt.Errorf("refuse callback")
				}
				return agent.Effects{}, nil
			}
			done := make(chan error, 1)
			go func() {
				_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
				done <- err
			}()
			// Control requires Started; use a timer event with a harmless timer notice.
			select {
			case <-entered:
			case err := <-done:
				require.FailNow(t, "callback not reached", fmt.Sprint(err))
			}
			r, err := s.worker.InterruptBridgeOutput(context.Background(), scope, "stop", 2, true)
			require.NoError(t, err)
			require.EqualValues(t, 2, r.CancelFloor)
			close(release)
			err = <-done
			if rollback {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 2, s.state.Agent.V2.CancelFloor)
			require.Empty(t, s.agent.v2.playback.bridgeQueue)
			require.EqualValues(t, 2, s.agent.v2.bridge.state.ClearFloor)
		})
	}
}

func TestBridgeOutputObserversDoNotGateIdleCompletion(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly}, {Step: "a", Kind: "bridge.audio/v1", Turn: 1, Class: agent.ReadOnly}}}, nil
		}
		return agent.Effects{}, nil
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
	require.NoError(t, err)
	require.Len(t, s.agent.v2.tasks, 2)
	require.Empty(t, s.agent.latest.V2.Capacity)
	s.launchTasks()
	defer s.stopTaskAttempts()
	submitBridge(t, s, scope, 1, 1)
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 1))
	paceBridge(t, s)
	require.NoError(t, s.deliverClockEvent())
	outcome, err := s.worker.BridgePlaybackOutcome(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "complete", outcome.Outcome)
	require.Len(t, s.agent.v2.tasks, 2)
	metadata, err := json.Marshal(s.state.Agent.V2)
	require.NoError(t, err)
	require.LessOrEqual(t, len(metadata), v2HostMetadataBytes)
}
func TestBridgeOutputExactExportKeepsApplicationLength(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	app := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 3}, payload: []byte("app")}
	s.agent.v2.instance = app
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	submitBridge(t, s, scope, 1, 1)
	s.publishAgent()
	s.mu.Lock()
	require.NoError(t, s.reserveV2ExportScratch())
	s.agent.paused = true
	s.mu.Unlock()
	first, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	second, err := s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Equal(t, first.bridge, second.bridge)
	require.Equal(t, []byte("app"), first.payload)
	require.Equal(t, 3, s.agent.latest.V2.MoveOnlyLength)
	payload := &movePayload{Bytes: first.payload, Tasks: first.tasks, Audio: first.audio, Bridge: first.bridge}
	data, err := s.checkpoint.capture(checkpointExact, payload)
	require.NoError(t, err)
	require.NoError(t, validateV2Wire(data))
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, 3, snap.State.Agent.V2.MoveOnlyLength)
	require.Equal(t, first.bridge, snap.AgentMoveOnly.Bridge)
}
func TestBridgeOutputPacerRejectsStaleScope(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	s.mu.Lock()
	v := cloneBridgeState(s.agent.v2.bridge.state)
	v.Scope.Generation++
	v.Scope.LineageID = "replacement"
	v.Revision++
	s.agent.v2.bridge.state = v
	s.mu.Unlock()
	paceBridge(t, s)
	require.Empty(t, s.agent.v2.playback.bridgeQueue)
	require.Equal(t, "canceled", s.agent.v2.bridge.records[1].Receipt.Outcome)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputActorPreservesQueuedAccounting(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	submitBridge(t, s, scope, 1, 2)
	before := s.agent.v2.playback.snapshot()
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	require.Equal(t, 320, s.agent.v2.playback.length)
	require.Equal(t, 40*time.Millisecond, s.agent.v2.playback.duration)
	require.True(t, playbackEqual(before, s.state.Agent.V2.Playback))
}
func TestBridgeOutputEncodedRangeBudget(t *testing.T) {
	ranges := []bridgeReceiptRange{{First: 1, Last: 9, Turn: 1, Outcome: "refused", Reason: "output_late"}, {First: ^uint64(0) - 2, Last: ^uint64(0) - 1, Turn: ^agent.Turn(0), Outcome: "refused", Reason: "backpressure"}}
	encoded, err := json.Marshal(ranges)
	require.NoError(t, err)
	require.Equal(t, len(encoded), bridgeRangesBytes(ranges))
}
func TestBridgeOutputStaleObserverEffectsDoNotCloseActor(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	_, err := s.worker.InterruptBridgeOutput(context.Background(), scope, "stop", 2, true)
	require.NoError(t, err)
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly}}}, nil
	}
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	require.Empty(t, s.agent.v2.tasks)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputCrashStatusCannotAuthorizeResend(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	saved := cloneAgentState(s.state.Agent)
	target, _ := newBridgeRig(t, false)
	dst := target.s
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.latest = cloneAgentState(saved)
	require.NoError(t, dst.restoreBridgeOutput(nil))
	status, err := dst.worker.BridgeOutputStatus(scope, 2)
	require.NoError(t, err)
	require.Equal(t, "unknown", status.Status)
	credit, err := dst.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Zero(t, credit.PacketSlots)
	resume, err := dst.worker.BridgeOutputResume(scope)
	require.NoError(t, err)
	require.True(t, resume.RequiresReconciliation)
	require.EqualValues(t, 1, resume.DurableThrough)
	require.Len(t, resume.Unknown, 1)
	sourceStatus, err := s.worker.BridgeOutputStatus(scope, 2)
	require.NoError(t, err)
	require.Equal(t, "unadmitted", sourceStatus.Status)
}
func TestBridgeOutputInvalidAudioHasTerminalIdentity(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	first, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 1, nil)
	require.NoError(t, err)
	require.Equal(t, "invalid_audio", first.Reason)
	again, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, 1, nil)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, "conflict", submitBridge(t, s, scope, 1, 1).Reason)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 2).Outcome)
	require.Len(t, s.agent.v2.playback.bridgeQueue, 1)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputClearAudioKeepsCancelFloor(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	receipt, err := s.worker.ClearBridgeAudio(context.Background(), scope, "clear", 2)
	require.NoError(t, err)
	require.Zero(t, receipt.CancelFloor)
	require.EqualValues(t, 2, receipt.ClearFloor)
	require.Equal(t, []uint64{1}, receipt.Canceled)
	require.Empty(t, s.agent.v2.playback.bridgeQueue)
	require.Zero(t, s.state.Agent.V2.CancelFloor)
}
func TestBridgeOutputLostReplyReconcilesBeforeRetry(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	ctx, cancel := context.WithCancel(context.Background())
	reply := make(chan error, 1)
	go func() { _, err := s.worker.SubmitBridgeOutput(ctx, scope, 1, 1, bridgePacket(160)); reply <- err }()
	request := <-s.agent.v2.bridge.ingress
	cancel()
	require.ErrorIs(t, <-reply, context.Canceled)
	receipt := s.admitBridgePacket(request)
	s.releaseBridgeIngress(request)
	request.done <- receipt
	status, err := s.worker.BridgeOutputStatus(scope, 1)
	require.NoError(t, err)
	require.Equal(t, "settled", status.Status)
	require.Equal(t, receipt, *status.Receipt)
	startBridgeLoop(t, s)
	require.Equal(t, receipt, submitBridge(t, s, scope, 1, 1))
	require.Len(t, s.agent.v2.playback.bridgeQueue, 1, "uncertain reply cannot cause replay")
}
func TestBridgeOutputObserverPressureBackpressuresWithoutRefusal(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	handler := func(context.Context, *agent.TaskRun) error { return nil }
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}
	specs := []agent.TaskSpec{{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly}, {Step: "a", Kind: "bridge.audio/v1", Turn: 1, Class: agent.ReadOnly}}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{StartTasks: specs}, nil
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "initial"}, eventLive)
	require.NoError(t, err)
	mailbox := s.agent.v2.registration.Bounds.MaxMailboxBytes
	s.agent.v2.registration.Bounds.MaxMailboxBytes = int(s.moveReserved)
	next, refused, err := s.prepareTaskEffects(*s.agent.latest.V2, agent.Event{Kind: agent.TimerFired, ID: "successor"}, agent.Effects{StartTasks: specs}, s.agent.v2.tasks, nil)
	s.agent.v2.registration.Bounds.MaxMailboxBytes = mailbox
	require.ErrorIs(t, err, errTaskAdmissionBackpressure)
	require.Empty(t, next)
	require.Empty(t, refused)
	require.Empty(t, s.agent.latest.V2.Capacity)
	require.Len(t, s.agent.v2.tasks, 2)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 1).Outcome)
}
func TestBridgeOutputReservesInterruptionMetadata(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	var turn agent.Turn = 2
	for ; turn < 100; turn++ {
		_, err := s.worker.BeginBridgeOutput(scope, turn)
		if err != nil {
			require.ErrorIs(t, err, agent.ErrBackpressure)
			break
		}
	}
	require.Less(t, turn, agent.Turn(100), "bounded markers preserve reserved interruption space")
	receipt, err := s.worker.InterruptBridgeOutput(context.Background(), scope, "stop", ^agent.Turn(0), true)
	require.NoError(t, err)
	require.Equal(t, ^agent.Turn(0), receipt.CancelFloor)
	s.mu.Lock()
	state := cloneBridgeState(s.agent.v2.bridge.state)
	playback := s.playbackSnapshotLocked()
	envelope := cloneAgentState(s.state.Agent)
	s.mu.Unlock()
	charge, err := bridgeMetadataCharge(state, playback)
	require.NoError(t, err)
	require.LessOrEqual(t, charge, bridgeMetadataPartition)
	data, err := json.Marshal(envelope.V2)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), v2HostMetadataBytes)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputIngressBudgetIsNotReset(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	ingress := time.Now().Add(-s.agent.v2.bridge.limits.AdmissionTimeout - time.Nanosecond)
	receipt, err := s.submitBridgeOutput(context.Background(), scope, 1, 1, bridgePacket(160), ingress)
	require.NoError(t, err)
	require.Equal(t, "output_late", receipt.Reason)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, 2).Outcome)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputRepliesAreScopedAndCreditsMove(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	first, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	receipt := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, scope, receipt.BridgeOutputScope)
	second, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Greater(t, second.Revision, first.Revision)
	require.Equal(t, scope, second.BridgeOutputScope)
	status, err := s.worker.BridgeOutputStatus(scope, 1)
	require.NoError(t, err)
	require.Equal(t, scope, status.BridgeOutputScope)
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"lineage_id":"lineage"`)
	s.mu.Lock()
	image := s.exportBridgeLocked()
	saved := cloneAgentState(s.state.Agent)
	s.mu.Unlock()
	target, _ := newBridgeRig(t, false)
	dst := target.s
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.latest = cloneAgentState(saved)
	require.NoError(t, dst.restoreBridgeOutput(&movePayload{Bridge: image}))
	moved, err := dst.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Greater(t, moved.Revision, second.Revision)
}
func TestBridgeOutputOldScopeCannotSupplyReceipts(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	paceBridge(t, s)
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 7)
	state := cloneBridgeState(s.agent.v2.bridge.state)
	state.Scope.Generation++
	state.Scope.LineageID = "new"
	s.agent.v2.bridge.state = state
	s.mu.Unlock()
	current := scope
	current.Generation++
	current.LineageID = "new"
	status, err := s.worker.BridgeOutputStatus(current, 1)
	require.NoError(t, err)
	require.Equal(t, "unknown", status.Status)
	require.Nil(t, status.Receipt)
	_, err = s.worker.BridgeOutputCheckpointed(current)
	require.ErrorIs(t, err, ErrBridgeOutputUncertain)
	resume, err := s.worker.BridgeOutputResume(current)
	require.NoError(t, err)
	require.Zero(t, resume.DurableThrough)
	require.Equal(t, "stale_scope", submitBridge(t, s, current, 1, 1).Reason)
}
func TestBridgeOutputExportScratchSharesSessionBudget(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	var next uint64
	for next = 1; next < 1000; next++ {
		credit, err := s.worker.BridgeOutputCredit(scope)
		require.NoError(t, err)
		if credit.PacketSlots == 0 || credit.PayloadBytes < 1200 {
			break
		}
		receipt, err := s.worker.SubmitBridgeOutput(context.Background(), scope, 1, next, bridgePacket(1200))
		require.NoError(t, err)
		require.Equal(t, "admitted", receipt.Outcome)
	}
	require.Less(t, next, uint64(1000))
	s.mu.Lock()
	err := s.reserveV2ExportScratch()
	paused := s.agent.paused
	release, _ := s.claimV2ExportScratch()
	s.mu.Unlock()
	require.NoError(t, err)
	defer release()
	require.False(t, paused)
	require.False(t, s.fenced.Load())
	paceBridge(t, s)
	require.Equal(t, "admitted", submitBridge(t, s, scope, 1, next).Outcome)
}

func TestBridgeOutputOwnerEpochGatesUnboundQueue(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	s.mu.Lock()
	before := s.state.Audio.Packets
	s.lease.Epoch++
	s.mu.Unlock()
	_, err := s.paceOne()
	require.NoError(t, err)
	require.Equal(t, before, s.state.Audio.Packets)
	require.Len(t, s.agent.v2.playback.bridgeQueue, 1)
	require.Equal(t, "admitted", s.agent.v2.bridge.records[1].Receipt.Outcome)
	_, err = s.worker.BridgeOutputCredit(scope)
	require.ErrorIs(t, err, ErrBridgeOutputUncertain)
	require.NoError(t, s.ctx.Err())
}
func TestBridgeOutputEmptyTurnHasNoUnknownPacketRange(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 0))
	_, err := s.worker.BeginBridgeOutput(scope, 2)
	require.NoError(t, err)
	submitBridge(t, s, scope, 2, 1)
	saved := cloneAgentState(s.state.Agent)
	target, _ := newBridgeRig(t, false)
	dst := target.s
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.latest = cloneAgentState(saved)
	require.NoError(t, dst.restoreBridgeOutput(nil))
	resume, err := dst.worker.BridgeOutputResume(scope)
	require.NoError(t, err)
	require.Len(t, resume.Unknown, 1)
	require.EqualValues(t, 2, resume.Unknown[0].Turn)
}
func TestBridgeOutputCrashKeepsOpenTurn(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	saved := cloneAgentState(s.state.Agent)
	target, _ := newBridgeRig(t, false)
	dst := target.s
	dst.state.Agent = cloneAgentState(saved)
	dst.agent.latest = cloneAgentState(saved)
	require.NoError(t, dst.restoreBridgeOutput(nil))
	require.False(t, dst.agent.v2.bridge.state.Turns[0].Ended, "a crash is not audio_end from the continuing bot")
	require.Equal(t, "unknown", dst.agent.v2.bridge.state.Turns[0].Outcome)
	resume, err := dst.worker.BridgeOutputResume(scope)
	require.NoError(t, err)
	require.True(t, resume.RequiresReconciliation)
	require.Len(t, resume.Unknown, 1)
}
