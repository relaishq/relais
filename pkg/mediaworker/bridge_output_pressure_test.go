package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func startBridgeActor(t *testing.T, s *session) {
	t.Helper()
	close(s.agent.v2.ready)
	done := make(chan struct{})
	go func() { defer close(done); s.v2AgentLoop() }()
	t.Cleanup(func() { s.cancel(); <-done; s.worker.running.Wait() })
}

func TestBridgeOutputReceiptPressurePreservesActor(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rig.wire.packets:
			}
		}
	}()
	t.Cleanup(func() { s.cancel(); <-drained })
	var refused uint64
	for seq := uint64(1); seq <= 5000; seq++ {
		r := s.admitBridgePacket(bridgeRequest(scope, seq, time.Now()))
		if r.Outcome != "admitted" {
			refused = seq
			break
		}
		paceBridge(t, s)
	}
	require.NotZero(t, refused, "no-pruning stream must stop at a bounded receipt ledger")
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"work": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
	rig.app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
		if e.Kind == agent.Started {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "w", Kind: "work", Turn: 2, Class: agent.ReadOnly, Request: make([]byte, 1500)}}}, nil
		}
		return agent.Effects{}, nil
	}
	startBridgeActor(t, s)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.agent.failed.Load() || (s.state.Agent.V2.Started && len(s.state.Agent.V2.Tasks) == 1)
	}, time.Second, time.Millisecond)
	require.False(t, s.agent.failed.Load(), "ordinary task event must not close the actor under retained receipt pressure")
	require.LessOrEqual(t, s.agent.v2.bridge.memory.Load(), int64((1<<20)-(160<<10)), "output must preserve actor headroom")
	// Arbitrarily many more output attempts cannot consume the reserve. The
	// actor keeps accepting ordinary clock events without pruning the ledger.
	for range 100 {
		require.Equal(t, "backpressure", s.admitBridgePacket(bridgeRequest(scope, refused, time.Now())).Reason)
	}
	require.NoError(t, s.ctx.Err())
	require.False(t, s.agent.failed.Load())
	credit, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Zero(t, credit.PacketSlots)
	retainedBytes := s.agent.v2.bridge.memory.Load()
	// Durable acknowledgement alone cannot prune an unacknowledged producer ID.
	s.mu.Lock()
	sent := s.agent.v2.bridge.state.Sent
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	s.mu.Unlock()
	credit, err = s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Zero(t, credit.PacketSlots)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, sent))
	credit, err = s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Positive(t, credit.PacketSlots)
	require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(scope, refused, time.Now())).Outcome)
	require.False(t, s.agent.failed.Load())
	require.NoError(t, s.ctx.Err())
	t.Logf("retained receipts=%d bridge_bytes=%d actor_headroom=%d", refused-1, retainedBytes, (1<<20)-retainedBytes)
}

func TestBridgeOutputActorPressureIsRetryable(t *testing.T) {
	rig, _ := newBridgeRig(t, false)
	s := rig.s
	require.NoError(t, s.reserveBridgeMemory((1<<20)-(160<<10)))
	err := s.reserveMoveBytes(200 << 10)
	require.ErrorIs(t, err, errTaskAdmissionBackpressure)
}

func TestBridgeOutputCheckpointAckPrunesKnownHorizon(t *testing.T) {
	rig, scope := newBridgeRig(t, true)
	s := rig.s
	submitBridge(t, s, scope, 1, 1)
	paceBridge(t, s)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 1))
	require.NotEmpty(t, s.agent.v2.bridge.records)
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	s.mu.Unlock()
	require.Empty(t, s.agent.v2.bridge.records)
	require.Zero(t, s.agent.v2.bridge.memory.Load())
	r := submitBridge(t, s, scope, 1, 1)
	require.Equal(t, "settled", r.Status)
}

func TestBridgeOutputCheckpointPrunesEmptyFinishedTurn(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	require.NoError(t, s.worker.EndBridgeOutput(scope, 1, 0))
	require.NoError(t, s.deliverClockEvent())
	require.True(t, s.agent.v2.bridge.state.Turns[0].Finished)
	require.NoError(t, s.worker.AdvanceBridgeReplayHorizon(scope, 0))
	s.mu.Lock()
	s.bridgeCheckpointedLocked(cloneAgentState(s.state.Agent), 1)
	s.mu.Unlock()
	require.Empty(t, s.agent.v2.bridge.state.Turns)
	before := s.agent.v2.bridge.state.Revision
	s.mu.Lock()
	s.pruneBridgeLocked()
	s.mu.Unlock()
	require.Equal(t, before, s.agent.v2.bridge.state.Revision, "idle pruning must not create a new checkpoint revision")
}

type bridgeRollbackAgent struct {
	*acceptedEventAgent
	restore func()
}

func (a *bridgeRollbackAgent) SaveMoveOnly(context.Context) ([]byte, error) {
	return make([]byte, 100), nil
}
func (a *bridgeRollbackAgent) RestoreMoveOnly(context.Context, []byte) error { return nil }
func (a *bridgeRollbackAgent) Restore(ctx context.Context, state agent.State) error {
	if a.restore != nil {
		a.restore()
	}
	return a.acceptedEventAgent.Restore(ctx, state)
}
func TestBridgeOutputRollbackReservationFailureIsTerminal(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		t.Run(fmt.Sprint("bridge=", bridge), func(t *testing.T) {
			var rig acceptedEventRig
			if bridge {
				rig, _ = newBridgeRig(t, false)
			} else {
				rig = newAcceptedEventRig(t, agenthost.Bounds{}, nil)
			}
			s := rig.s
			rig.app.state.HasMoveOnly, rig.app.state.MoveOnlyLength = true, 100
			s.agent.v2.instance = &bridgeRollbackAgent{acceptedEventAgent: rig.app}
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.NoError(t, err)
			a := &bridgeRollbackAgent{acceptedEventAgent: rig.app}
			s.agent.v2.instance = a
			var pressure int64
			a.restore = func() {
				require.NoError(t, s.reserveMoveBytes(0))
				pressure = s.snapshotLimits().MoveBytesTotal - s.worker.moveBytes.Load()
				require.True(t, s.worker.reserveMoveBytes(pressure))
			}
			defer func() { s.worker.moveBytes.Add(-pressure) }()
			rig.app.save = func(context.Context) error { return errors.New("application save failed") }
			_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.TimerFired}, eventLive)
			if bridge {
				require.ErrorIs(t, err, legacy.ErrRestore, "rollback cannot recover accepted reservations")
			} else {
				require.ErrorIs(t, err, ErrMoveBudget, "other agents keep their existing terminal budget error")
				require.NotErrorIs(t, err, legacy.ErrRestore)
			}
			require.NotErrorIs(t, err, errTaskAdmissionBackpressure, "an unusable rollback is terminal")

		})
	}
}
func fillBridgeReceiptLedger(t *testing.T, rig acceptedEventRig, scope BridgeOutputScope) {
	t.Helper()
	s := rig.s
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rig.wire.packets:
			}
		}
	}()
	t.Cleanup(func() { s.cancel(); <-done })
	for seq := uint64(1); seq < 5000; seq++ {
		if r := s.admitBridgePacket(bridgeRequest(scope, seq, time.Now())); r.Outcome != "admitted" {
			return
		}
		paceBridge(t, s)
	}
	t.Fatal("receipt ledger did not bound output")
}
func TestBridgeOutputRetainedReceiptsBoundActorPressure(t *testing.T) {
	for _, size := range []int{150 << 10, 300 << 10, 900 << 10} {
		t.Run(fmt.Sprint("task-", size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			fillBridgeReceiptLedger(t, rig, scope)
			rig.s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"work": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
			rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
				return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "w", Kind: "work", Turn: 2, Class: agent.ReadOnly, Request: make([]byte, size)}}}, nil
			}
			_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.True(t, err == nil || errors.Is(err, errTaskAdmissionBackpressure), "fatal actor error: %v", err)
			require.NoError(t, rig.s.ctx.Err())
		})
	}
	for _, size := range []int{100 << 10, 200 << 10, 600 << 10} {
		t.Run(fmt.Sprint("move-", size), func(t *testing.T) {
			rig, scope := newBridgeRig(t, false)
			fillBridgeReceiptLedger(t, rig, scope)
			rig.s.agent.v2.instance = &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("a"), HasMoveOnly: true, MoveOnlyLength: size}, payload: make([]byte, size)}
			_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
			require.True(t, err == nil || errors.Is(err, errTaskAdmissionBackpressure), "fatal actor error: %v", err)
			require.NoError(t, rig.s.ctx.Err())
		})
	}
	t.Run("control", func(t *testing.T) {
		rig, scope := newBridgeRig(t, false)
		fillBridgeReceiptLedger(t, rig, scope)
		_, err := (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		require.NoError(t, err)
		rig.s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"work": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
		rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "c", Kind: "work", Turn: 2, Class: agent.ReadOnly, Request: make([]byte, 200<<10)}}}, nil
		}
		_, err = (acceptedEvents{rig.s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "c1", Sequence: 1, Name: "x"}}, eventLive)
		require.True(t, err == nil || controlRefusal(err), "fatal control error: %v", err)
		require.NoError(t, rig.s.ctx.Err())
	})
}
