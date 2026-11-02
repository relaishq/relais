package mediaworker

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type restartTestAgent struct {
	*acceptedEventAgent
	restored                  agent.State
	session                   *session
	acceptedUnderSnapshotLock bool
}

func (a *restartTestAgent) Restart(_ context.Context, state agent.State, _ agenthost.BridgeScope, _ string) (agent.State, error) {
	a.restored = cloneV2State(state)
	a.state = cloneV2State(state)
	return a.state, nil
}
func (a *restartTestAgent) Accepted() {
	if a.session != nil {
		if a.session.checkpoint.snapshotMu.TryLock() {
			a.session.checkpoint.snapshotMu.Unlock()
		} else {
			a.acceptedUnderSnapshotLock = true
		}
	}
}
func (*restartTestAgent) Checkpointed(agenthost.BridgeCheckpoint) {}
func (*restartTestAgent) Closed()                                 {}

func restartRig(t *testing.T, store sessionstore.Store) (*session, BridgeOutputScope, *restartTestAgent) {
	t.Helper()
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	lease, err := store.Claim(context.Background(), s.id, s.worker.LocalAddr(), time.Hour)
	require.NoError(t, err)
	s.lease = lease
	scope.Epoch = lease.Epoch
	s.agent.v2.bridge.state.Scope = bridgeOutputIdentity(scope)
	s.worker.cfg.Relay.Owners = store
	s.worker.cfg.SplitCheckpoints = true
	s.storeClock = time.Now()
	s.storeClockAt = time.Now()
	app := &restartTestAgent{acceptedEventAgent: rig.app, session: s}
	s.agent.v2.instance = app
	s.mu.Lock()
	s.projectBridgeLocked(s.state.Agent.V2)
	s.agent.latest = cloneAgentState(s.state.Agent)
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	return s, scope, app
}
func TestBridgeRestartInvalidatesQueuedInterruptionAcrossExactMove(t *testing.T) {
	forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
		s, old, app := restartRig(t, store)
		// A state observer has an old checkpoint queued in its real host mailbox.
		s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": func(context.Context, *agent.TaskRun) error { return nil }}
		app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
			if e.Kind == agent.Started {
				return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "state", Kind: "bridge.state/v1", Class: agent.ReadOnly, Resume: agent.Abandon}}}, nil
			}
			return agent.Effects{}, nil
		}
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "state-before-replacement"}, eventLive)
		require.NoError(t, err)
		task := s.agent.v2.tasks[0]
		emission := taskEmission{id: task.intent.ID, attempt: task.attempt, ctx: s.ctx, sequence: 1, chunk: agent.Chunk{Data: []byte("obsolete checkpoint")}, reply: make(chan error, 1)}
		s.acceptEmission(emission)
		require.NoError(t, <-emission.reply)
		require.Len(t, task.results, 1)
		// The packet and interruption also belong to the old generation.
		require.Equal(t, "admitted", s.admitBridgePacket(bridgeRequest(old, 1, time.Now())).Outcome)
		request := bridgePriorityRequest{scope: old, requestID: "queued-before-replacement", floor: 99, cancel: true, clear: true, done: make(chan bridgePriorityReply, 1)}
		s.agent.v2.bridge.priority <- request
		_, err = s.worker.StopBridgePermits(old)
		require.NoError(t, err)
		require.NoError(t, s.worker.CheckpointBridgeStop(context.Background(), old))
		next := old
		next.Generation++
		next.LineageID = "successor"
		require.NoError(t, s.restartBridge(bridgeRestartRequest{ctx: context.Background(), old: old, next: next, runner: "runner"}))
		require.Equal(t, []byte("accepted"), app.restored.Bytes)
		require.Empty(t, s.agent.v2.bridge.permit.stopped)
		require.Equal(t, int64(0), s.agent.v2.bridge.memory.Load())
		image, err := s.saveV2MoveOnly(context.Background())
		require.NoError(t, err)
		require.Empty(t, image.tasks, "replacement must remove the queued obsolete checkpoint from exact export")
		data, err := s.checkpoint.capture(checkpointExact, &movePayload{Bridge: image.bridge})
		require.NoError(t, err)
		snap, err := decodeSnapshot(data)
		require.NoError(t, err)
		dstRig, _ := newBridgeRig(t, false)
		dst := dstRig.s
		dst.state.Agent = cloneAgentState(snap.State.Agent)
		dst.agent.latest = cloneAgentState(snap.State.Agent)
		require.NoError(t, dst.restoreBridgeOutput(snap.AgentMoveOnly))
		dst.lease.Epoch = next.Epoch + 1
		moved := next
		moved.Epoch = dst.lease.Epoch
		require.NoError(t, dst.worker.ReattachBridgeOutput(moved, 0))
		dst.applyBridgePriority(<-s.agent.v2.bridge.priority)
		require.ErrorIs(t, (<-request.done).err, ErrBridgeOutputScope)
		require.Zero(t, dst.agent.v2.bridge.state.CancelFloor)
		require.Zero(t, dst.agent.v2.bridge.state.ClearFloor)
		require.Empty(t, dst.agent.v2.playback.bridgeQueue, "old audio cannot return through exact export")
		require.EqualValues(t, 0, dst.agent.v2.bridge.state.Through, "successor starts a fresh packet identity space")
	})
}

// Store commits, loses its reply and refuses settlement until Transfer wins.
// Restart must observe the new tenure, never activate against a stale tuple.
type restartLostReply struct {
	sessionstore.Store
	sessionstore.ComponentStore
	lose       bool
	unreadable bool
}

func (s *restartLostReply) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	tuple, err := s.ComponentStore.CommitComponents(ctx, l, c)
	if s.lose && err == nil {
		s.lose = false
		s.unreadable = true
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "lost_reply", Err: context.DeadlineExceeded}
	}
	return tuple, err
}
func (s *restartLostReply) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	if s.unreadable {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "settlement", Err: context.DeadlineExceeded}
	}
	return s.ComponentStore.ReadComponents(ctx, id)
}
func TestBridgeRestartTakeoverWinsUncertainStopAndRestoresStopBoundary(t *testing.T) {
	forV2Stores(t, func(t *testing.T, base sessionstore.Store) {
		store := &restartLostReply{Store: base, ComponentStore: base.(sessionstore.ComponentStore)}
		s, old, _ := restartRig(t, store)
		_, err := s.worker.StopBridgePermits(old)
		require.NoError(t, err)
		store.lose = true
		require.ErrorIs(t, s.worker.CheckpointBridgeStop(context.Background(), old), sessionstore.ErrTransient)
		require.NotNil(t, s.checkpoint.componentPending)
		owner := s.worker.LocalAddr()
		owner = netip.AddrPortFrom(owner.Addr(), owner.Port()+1)
		lease, err := base.Transfer(context.Background(), s.lease, owner, time.Hour)
		require.NoError(t, err)
		store.unreadable = false
		next := old
		next.Epoch = old.Epoch
		next.Generation++
		next.LineageID = "successor"
		require.ErrorIs(t, s.restartBridge(bridgeRestartRequest{ctx: context.Background(), old: old, next: next, runner: "runner"}), sessionstore.ErrLeaseLost)
		require.Equal(t, old, BridgeOutputScope(s.agent.v2.bridge.state.Scope))
		data, _, err := ReadRecoverySnapshot(context.Background(), base, s.id)
		require.NoError(t, err)
		cfg := s.worker.cfg
		cfg.Relay.Owners = base
		cfg.Relay.LeaseTTL = time.Hour
		cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "bridge", Factory: func(agent.FactoryArgs) agent.Agent {
			return &restartTestAgent{acceptedEventAgent: &acceptedEventAgent{}}
		}})
		target, _ := bridgeResumeWorker(cfg, owner)
		target.paused.Store(true)
		t.Cleanup(func() { stopBridgeResumeWorker(target, s.id) })
		_, err = target.ResumeSession(data, ResumeOptions{Lease: lease, Kind: legacy.Takeover})
		require.NoError(t, err)
		restored := target.session(s.id)
		require.True(t, restored.agent.v2.bridge.state.Stopped)
		reconnect := old
		reconnect.Epoch = lease.Epoch
		require.ErrorIs(t, target.ReattachBridgeOutput(reconnect, 0), ErrBridgeRevoked)
		require.Zero(t, target.BridgePermitRemaining(old))
		// The owner seam is exercised after the independent supervisor gate tests.
		// Activation still needs this owner's fenced lineage commit and renewal.
		successor := old
		successor.Epoch = lease.Epoch
		successor.Generation++
		successor.LineageID = "recovered-successor"
		require.NoError(t, target.RestartBridge(context.Background(), old, successor, "runner"))
		_, err = target.BridgeOutputCheckpointed(successor)
		require.NoError(t, err)
		restored.mu.Lock()
		restored.cacheBridgeRenewal(successor, time.Now(), true)
		restored.mu.Unlock()
		require.Positive(t, target.BridgePermitRemaining(successor))
		require.Zero(t, target.BridgePermitRemaining(old))
	})
}

func TestBridgeHistoryPressureDefersObserverInsteadOfTerminating(t *testing.T) {
	forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
		s, _, app := restartRig(t, store)
		s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": func(context.Context, *agent.TaskRun) error { return nil }}
		app.handle = func(_ context.Context, e agent.Event) (agent.Effects, error) {
			if e.Kind == agent.Started {
				return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "state", Kind: "bridge.state/v1", Class: agent.ReadOnly, Resume: agent.Abandon}}}, nil
			}
			if e.Kind == agent.TaskMessage {
				app.state.History = make([]byte, 8192)
			}
			return agent.Effects{}, nil
		}
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "started"}, eventLive)
		require.NoError(t, err)
		task := s.agent.v2.tasks[0]
		emission := taskEmission{id: task.intent.ID, attempt: task.attempt, ctx: s.ctx, sequence: 1, chunk: agent.Chunk{Data: []byte("candidate")}, reply: make(chan error, 1)}
		s.acceptEmission(emission)
		require.NoError(t, <-emission.reply)
		restoreCheckpointQuota(t, s, 0, s.taskNow())
		delivered, err := s.deliverTaskResult()
		require.NoError(t, err)
		require.False(t, delivered)
		require.False(t, task.intent.Final, "history pressure must retain the state observer")
		require.Len(t, task.results, 1)
		require.Empty(t, s.state.Agent.History, "unadmitted history is not the accepted pair")
		restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
		delivered, err = s.deliverTaskResult()
		require.NoError(t, err)
		require.True(t, delivered)
		require.Len(t, s.state.Agent.History, 8192)
	})
}

type lateConversationStore struct {
	sessionstore.Store
	sessionstore.ComponentStore
	late             *componentSnapshot
	hold, unreadable bool
}

func (s *lateConversationStore) CommitComponents(ctx context.Context, l sessionstore.Lease, c sessionstore.ComponentCommit) (sessionstore.ComponentTuple, error) {
	if s.late != nil {
		late := s.late
		s.late = nil
		if s.hold {
			s.unreadable = true
		}
		_, err := s.ComponentStore.CommitComponents(ctx, l, sessionstore.ComponentCommit{Components: late.Components, StateVersion: late.StateVersion, Expected: c.Expected, WriteBudgetBytes: c.WriteBudgetBytes})
		if err != nil {
			return sessionstore.ComponentTuple{}, err
		}
	}
	return s.ComponentStore.CommitComponents(ctx, l, c)
}
func (s *lateConversationStore) ReadComponents(ctx context.Context, id string) (sessionstore.ComponentTuple, error) {
	if s.unreadable {
		return sessionstore.ComponentTuple{}, &sessionstore.TransientError{Op: "settle_late", Err: context.DeadlineExceeded}
	}
	return s.ComponentStore.ReadComponents(ctx, id)
}
func TestBridgeRestartReconcilesAnOldWriteThatWinsAfterBaselineChoice(t *testing.T) {
	testBridgeLateWrite(t, false)
}
func TestBridgeRestartPausesBackgroundWritesUntilBaselineReconciliation(t *testing.T) {
	testBridgeLateWrite(t, true)
}
func testBridgeLateWrite(t *testing.T, hold bool) {
	forV2Stores(t, func(t *testing.T, base sessionstore.Store) {
		store := &lateConversationStore{Store: base, ComponentStore: base.(sessionstore.ComponentStore), hold: hold}
		s, old, app := restartRig(t, store)
		_, err := s.worker.StopBridgePermits(old)
		require.NoError(t, err)
		require.NoError(t, s.worker.CheckpointBridgeStop(context.Background(), old))
		app.state.Bytes = []byte("late old conversation")
		app.state.History = []byte("late matching history")
		_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "late-capture"}, eventLive)
		require.NoError(t, err)
		data, _, _, err := s.checkpoint.captureSnapshotCandidateMode(time.Now(), nil, true)
		require.NoError(t, err)
		var late componentSnapshot
		require.NoError(t, json.Unmarshal(data, &late))
		store.late = &late
		next := old
		next.Generation++
		next.LineageID = "successor"
		err = s.restartBridge(bridgeRestartRequest{ctx: context.Background(), old: old, next: next, runner: "runner"})
		if hold {
			require.ErrorIs(t, err, sessionstore.ErrTransient)
			store.unreadable = false
			require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointRequired), errCheckpointSkipped)
			require.Zero(t, s.worker.BridgePermitRemaining(next))
			request := <-s.agent.v2.bridge.restarts
			require.NoError(t, s.restartBridge(request))
		} else if err != nil {
			require.ErrorIs(t, err, sessionstore.ErrStateSuperseded)
			require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
		}
		require.False(t, app.acceptedUnderSnapshotLock, "accepted notifications must run after unlocking checkpoint serialization")
		saved, _, err := ReadRecoverySnapshot(context.Background(), base, s.id)
		require.NoError(t, err)
		snapshot, err := decodeSnapshot(saved)
		require.NoError(t, err)
		require.Equal(t, next, BridgeOutputScope(snapshot.State.Agent.V2.Bridge.Scope))
		require.Equal(t, []byte("late old conversation"), snapshot.State.Agent.State.Bytes)
		require.Equal(t, []byte("late matching history"), snapshot.State.Agent.History)
	})
}

func TestBridgeActivationExportRefusalRemainsRetryable(t *testing.T) {
	for _, path := range []string{"preflight", "flush"} {
		t.Run(path, func(t *testing.T) {
			rig, _ := newBridgeRig(t, false)
			s := rig.s
			b := s.agent.v2.bridge
			b.restart = &bridgeRestartRequest{}
			b.permit.stopped = time.Now()
			var err error
			if path == "preflight" {
				err = s.reserveV2ExportScratch()
			} else {
				request := agentFlush{done: make(chan error, 1), moveOnly: make(chan agentMoveExport, 1)}
				require.True(t, s.finishAgentFlush(request))
				err = <-request.done
			}
			require.ErrorIs(t, err, ErrExportNotFlushed, "activation is temporary, not a revoked generation")
			require.NotErrorIs(t, err, ErrBridgeRevoked)
		})
	}
}

type lateRestartAgent struct {
	*restartTestAgent
	expired, release, returned chan struct{}
	restored                   chan bool
}

func (a *lateRestartAgent) Restart(ctx context.Context, state agent.State, _ agenthost.BridgeScope, _ string) (agent.State, error) {
	<-ctx.Done()
	close(a.expired)
	<-a.release
	close(a.returned)
	return state, nil
}
func (a *lateRestartAgent) Restore(ctx context.Context, state agent.State) error {
	select {
	case <-a.returned:
		a.restored <- true
	default:
		a.restored <- false
	}
	return a.restartTestAgent.Restore(ctx, state)
}
func TestBridgeRestartRollbackWaitsForLateCallback(t *testing.T) {
	s, old, app := restartRig(t, sessionstore.NewMemory())
	_, err := s.worker.StopBridgePermits(old)
	require.NoError(t, err)
	require.NoError(t, s.worker.CheckpointBridgeStop(context.Background(), old))
	s.worker.cfg.Agent.CallbackTimeout = 500 * time.Millisecond
	late := &lateRestartAgent{restartTestAgent: app, expired: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{}), restored: make(chan bool, 1)}
	s.agent.v2.instance = late
	next := old
	next.Generation++
	next.LineageID = "successor"
	done := make(chan error, 1)
	go func() {
		done <- s.restartBridge(bridgeRestartRequest{ctx: context.Background(), old: old, next: next, runner: "runner"})
	}()
	<-late.expired
	var early *bool
	select {
	case result := <-late.restored:
		early = &result
	case <-s.agent.late:
	}
	close(late.release)
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	if early != nil {
		require.True(t, *early, "rollback Restore overlapped a timed-out Restart")
	} else {
		require.True(t, <-late.restored)
	}
}
