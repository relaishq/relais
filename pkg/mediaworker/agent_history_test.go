package mediaworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func historyUnitSession(t *testing.T, a agent.Agent, limits agent.Limits) (*Worker, *session) {
	t.Helper()
	w, s := v2UnitSession(t, a, func(c *AgentConfig) {
		c.Factory = agenthost.Factory(agenthost.Registration{Name: "unit/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }, Bounds: agenthost.Bounds(limits)})
	})
	s.componentMode = true
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	return w, s
}

func TestSDKHistorySaveRestorePair(t *testing.T) {
	original := agent.State{Version: 1, Bytes: []byte("working"), History: []byte("previous user: 你好\x00\xff; previous reply")}
	a := &v2UnitAgent{state: cloneV2State(original)}
	_, source := historyUnitSession(t, a, agent.Limits{})
	require.Equal(t, original.History, source.state.Agent.History)
	// App-owned Save buffers cannot mutate the accepted pair.
	a.state.Bytes[0], a.state.History[0] = 'X', 'X'
	require.Equal(t, original.Bytes, source.state.Agent.State.Bytes)
	require.Equal(t, original.History, source.state.Agent.History)
	snap := componentFixture(t)
	snap.State.Agent = cloneAgentState(source.state.Agent)
	snap.split = &componentSnapshotState{history: bytes.Clone(source.state.Agent.History)}
	data, err := json.Marshal(snap)
	require.NoError(t, err)
	decoded, err := decodeSnapshot(data)
	require.NoError(t, err)
	b := &v2UnitAgent{state: agent.State{Version: 1}, callback: func(context.Context) {}}
	_, target := v2UnitSession(t, b)
	target.state = decoded.State
	require.NoError(t, target.checkpoint.restore(checkpointRestore{snapshot: decoded}))
	require.NoError(t, target.initAgent(true, ResumeOptions{Kind: legacy.Takeover}))
	require.Equal(t, original.History, b.state.History)
	require.Equal(t, original.History, target.state.Agent.History)
	require.Equal(t, legacy.Takeover, b.lastEvent.Resume.Kind)
	require.True(t, target.componentMode)
	require.False(t, target.worker.cfg.SplitCheckpoints)
}

func TestSDKHistoryDefaultProfiles(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			t.Run(map[bool]string{true: "split", false: "legacy"}[split]+map[bool]string{true: "-explicit", false: "-zero"}[explicit], func(t *testing.T) {
				var args agent.FactoryArgs
				limits := agent.Limits{}
				if explicit {
					limits.MaxStateBytes = 2048
					limits.MaxHistoryBytes = 1024
				}
				a := &v2UnitAgent{state: agent.State{Version: 1}}
				w, s := v2UnitSession(t, a, func(c *AgentConfig) {
					c.Factory = agenthost.Factory(agenthost.Registration{Name: "unit/v1", Factory: func(v agent.FactoryArgs) agent.Agent { args = v; return a }, Bounds: agenthost.Bounds(limits)})
				})
				s.componentMode = split
				// A worker with split admission on must still restore legacy profiles.
				w.cfg.SplitCheckpoints = true
				require.NoError(t, s.initAgent(false, ResumeOptions{}))
				working, history := 1024, 0
				if split {
					working, history = 3072, 8192
				}
				if explicit {
					working = 2048
					if split {
						history = 1024
					}
				}
				require.Equal(t, working, args.MaxStateBytes)
				require.Equal(t, history, args.MaxHistoryBytes)
				require.Equal(t, limits, *s.state.Agent.V2.Limits, "declared identity stays unchanged")
				if !split {
					saved := s.state
					saved.Agent = cloneAgentState(saved.Agent)
					s.state = saved
					require.NoError(t, s.initAgent(true, ResumeOptions{Kind: legacy.Takeover}))
					require.Equal(t, working, args.MaxStateBytes)
					require.Equal(t, history, args.MaxHistoryBytes)
				}
			})
		}
	}
}

func TestSDKHistoryLimitRefusesPair(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted"), History: []byte("prior")}}
	_, s := historyUnitSession(t, a, agent.Limits{MaxHistoryBytes: 8})
	require.ErrorIs(t, s.validateV2StateMetadata(agent.State{Version: 1, History: make([]byte, 9)}), agent.ErrHistoryTooLarge)
	a.callback = func(context.Context) { a.state.History = []byte("too large") }
	input := legacy.Input{Progress: legacy.Progress{SSRC: 42, Index: 7}}
	_, eventErr0 := (acceptedEvents{s}).submit(agent.Event{ID: "input", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr0)
	require.Equal(t, []byte("prior"), s.state.Agent.History)
	require.Equal(t, []byte("accepted"), s.state.Agent.State.Bytes)
	require.Equal(t, []byte("prior"), a.state.History, "Restore rolls back both fields")
	require.Len(t, s.state.Agent.V2.Capacity, 1)
	require.Equal(t, agent.Capacity, s.state.Agent.V2.Capacity[0].result().Outcome)
	require.NoError(t, s.ctx.Err())
}

func TestSDKHistoryChurnRefusesBeforeAcceptance(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted"), History: bytes.Repeat([]byte{'a'}, 8192)}}
	_, s := historyUnitSession(t, a, agent.Limits{})
	limits := s.snapshotLimits()
	now := s.taskNow()
	restoreCheckpointQuota(t, s, float64(limits.MaxBytes+17), now)
	a.callback = func(context.Context) { a.state.History = bytes.Repeat([]byte{'b'}, 8192) }
	input := legacy.Input{Progress: legacy.Progress{SSRC: 42, Index: 7}}
	_, eventErr1 := (acceptedEvents{s}).submit(agent.Event{ID: "input", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr1)
	require.Equal(t, bytes.Repeat([]byte{'a'}, 8192), s.state.Agent.History)
	require.Len(t, s.state.Agent.V2.Capacity, 1)
	require.NoError(t, s.ctx.Err())
	// The refusal is delivered in its own transaction, then an ordinary input
	// continues without changing history or slowing transport cadence.
	a.callback = nil
	_, err := s.deliverTaskResult()
	require.NoError(t, err)
	require.Empty(t, s.state.Agent.V2.Capacity)
	_, eventErr2 := (acceptedEvents{s}).submit(agent.Event{ID: "next-input", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr2)
	require.EqualValues(t, 2, s.state.Agent.Progress.Consumed)
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	a.callback = func(context.Context) { a.state.History = bytes.Repeat([]byte{'c'}, 8192) }
	_, eventErr3 := (acceptedEvents{s}).submit(agent.Event{ID: "refilled-input", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr3)
	require.Equal(t, bytes.Repeat([]byte{'c'}, 8192), s.state.Agent.History)
}

func TestSDKHistoryLegacyRefusesNonempty(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	require.ErrorIs(t, s.validateV2StateMetadata(agent.State{Version: 1, History: []byte("unsupported")}), agent.ErrHistoryTooLarge)
}

func TestSplitPayloadBudgetAndHotReservation(t *testing.T) {
	l := SnapshotLimits{}.splitDefaults()
	require.Equal(t, 80000, l.BytesPerSecond)
	require.Equal(t, 40960, l.BurstBytes)
	require.NoError(t, l.Validate())
	require.Equal(t, 81920, SnapshotLimits{}.defaults().BytesPerSecond)
	require.Equal(t, 28672, SnapshotLimits{}.defaults().BurstBytes)
	explicit := SnapshotLimits{MaxBytes: 10000, BytesPerSecond: 120000, BurstBytes: 60000}.splitDefaults()
	require.Equal(t, 120000, explicit.BytesPerSecond)
	require.Equal(t, 60000, explicit.BurstBytes)
	require.NoError(t, explicit.Validate())
}

func TestSplitColdWritesPreserveHotAndDebitMetadata(t *testing.T) {
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, true)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointRequired))
	first, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().MaxBytes+100), time.Now())
	err = s.checkpoint.persist(context.Background(), checkpointIntent)
	require.ErrorIs(t, err, errCheckpointSkipped)
	unchanged, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, first.Metadata, unchanged.Metadata, "refused barrier publishes nothing")
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointIntent))
	second, err := store.ReadComponents(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, first.Metadata.References[2], second.Metadata.References[2], "hot writes retain history ciphertext")
	require.Greater(t, second.Metadata.References[4].Revision, first.Metadata.References[4].Revision)
	var tr transportComponent
	require.NoError(t, unmarshalComponent(second.Components.Transport, componentTransport, &tr))
	var host v2Envelope
	require.NoError(t, json.Unmarshal(tr.Host, &host))
	balance, err := host.Quota.balance()
	require.NoError(t, err)
	packed, err := componentMetadata(second.Metadata).marshal()
	require.NoError(t, err)
	charge := len(second.Components.Transport) + componentCipherOverhead + len(packed) + componentCommitTimeBytes + componentFieldNameBytes + componentProtocolReserve
	require.EqualValues(t, s.snapshotLimits().BurstBytes-charge, balance)
	require.GreaterOrEqual(t, checkpointQuota(t, s), balance, "live quota can refill after the exact stored debit")
	require.GreaterOrEqual(t, balance, uint64(s.snapshotLimits().MaxBytes))
}

func TestSplitSaveAndCaptureAgreeAtEncodedBoundary(t *testing.T) {
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	setComponentTestHistory(s, bytes.Repeat([]byte{'h'}, 8192))
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{MaxStateBytes: 64 << 10}, s.snapshotLimits(), true)}}}
	candidate := cloneAgentState(s.state.Agent)
	low, high := 0, 8193
	for low+1 < high {
		mid := (low + high) / 2
		candidate.State.Bytes = make([]byte, mid)
		_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
		if err != nil {
			require.ErrorIs(t, err, agent.ErrCapacity)
			high = mid
		} else {
			low = mid
		}
	}
	require.Positive(t, low)
	candidate.State.Bytes = make([]byte, low)
	_, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err)
	err = persistCheckpointCandidate(t, s, candidate)
	require.NoError(t, err)
	candidate.State.Bytes = make([]byte, low+1)
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.ErrorIs(t, err, agent.ErrCapacity)
	err = persistCheckpointCandidate(t, s, candidate)
	require.ErrorIs(t, err, agent.ErrCapacity)
	// Larger explicitly configured components require matching actual budgets.
	s.worker.splitLimits = SnapshotLimits{MaxBytes: 16384, BytesPerSecond: 200000, BurstBytes: 80000}.splitDefaults()
	s.agent.v2.registration.Bounds.MaxHistoryBytes = 12288
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	candidate.State.Bytes = make([]byte, 8192)
	candidate.History = make([]byte, 12288)
	require.NoError(t, s.validateV2StateMetadata(v2State(candidate)))
	_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: s.agent.latest})
	require.NoError(t, err)
	err = persistCheckpointCandidate(t, s, candidate)
	require.NoError(t, err)
	require.Equal(t, 8192, len(candidate.State.Bytes))
	require.Equal(t, 12288, len(candidate.History))
}

type historyStateAgent struct{ state agent.State }

func (a *historyStateAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (a *historyStateAgent) Save(context.Context) (agent.State, error) {
	return cloneV2State(a.state), nil
}
func (a *historyStateAgent) Restore(_ context.Context, state agent.State) error {
	a.state = cloneV2State(state)
	return nil
}

func TestSplitPlannedMoveCarriesQuotaAndHistory(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	history := []byte("opaque conversation across a planned move")
	factory := agenthost.Factory(agenthost.Registration{Name: "history-quota/v1", Factory: func(agent.FactoryArgs) agent.Agent {
		return &historyStateAgent{state: agent.State{Version: 1, Bytes: []byte("work"), History: history}}
	}})
	cfg := Config{SplitCheckpoints: true, Agent: AgentConfig{Factory: factory}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}}
	source, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	cfg.SplitCheckpoints = false
	target, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	r.AddWorker(source.LocalAddr())
	call, client := dialDTLSCaller(t, source)
	defer client.Close()
	s := source.session(call.id)
	require.Eventually(t, func() bool { return checkpointKnown(s) }, 2*time.Second, time.Millisecond)
	now := s.taskNow()

	s.checkpoint.wait()
	restoreCheckpointQuota(t, s, 22000, now)
	data, err := source.ExportSession(call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, history, snap.State.Agent.History)
	exported, err := snap.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.Less(t, exported, uint64(25000))
	lease, err := store.Get(context.Background(), call.id)
	require.NoError(t, err)
	lease, err = store.Transfer(context.Background(), lease, target.LocalAddr(), time.Minute)
	require.NoError(t, err)
	_, err = target.ResumeSession(data, ResumeOptions{Lease: lease})
	require.NoError(t, err)
	resumed := target.session(call.id)
	require.True(t, resumed.componentMode)
	resumed.mu.Lock()
	require.Equal(t, history, resumed.state.Agent.History)
	resumed.mu.Unlock()
	remaining := float64(checkpointQuota(t, resumed))
	require.Less(t, remaining, float64(25000), "the bridge cannot mint a fresh 40960-byte burst")
	tuple, err := store.ReadComponents(context.Background(), call.id)
	require.NoError(t, err)
	require.Equal(t, target.LocalAddr().String(), tuple.Metadata.Owner)
}

type historyChurnAgent struct{ historyStateAgent }

func (a *historyChurnAgent) Handle(_ context.Context, event agent.Event) (agent.Effects, error) {
	if event.Kind == agent.Control {
		a.state.History = bytes.Repeat(event.Control.Data, 8192)
	}
	return agent.Effects{}, nil
}

func TestSplitHistoryChurnKeepsTransportCheckpoints(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	factory := agenthost.Factory(agenthost.Registration{Name: "history-churn/v1", Factory: func(agent.FactoryArgs) agent.Agent {
		return &historyChurnAgent{historyStateAgent{state: agent.State{Version: 1, History: bytes.Repeat([]byte{'a'}, 8192)}}}
	}})
	w, err := New(Config{SplitCheckpoints: true, Agent: AgentConfig{Factory: factory, CallbackTimeout: time.Second}, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	r.AddWorker(w.LocalAddr())
	call, client := dialDTLSCaller(t, w)
	defer client.Close()
	s := w.session(call.id)
	require.Eventually(t, func() bool { return checkpointKnown(s) }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Agent.V2.Started }, 2*time.Second, time.Millisecond)
	w.paused.Store(true)

	s.checkpoint.wait()
	now := s.taskNow()
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().MaxBytes+17), now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// One refused sequence never adds receipt pressure. Each refusal here is
	// caused by the history byte gate, not the separate Control receipt window.
	for i := range 8 {
		_, err := w.AgentControl(ctx, call.id, agent.ControlMessage{ID: fmt.Sprint(i), Sequence: 1, Name: "remember", Data: []byte{'b'}})
		require.ErrorIs(t, err, agent.ErrCapacity)
		s.mu.Lock()
		require.Equal(t, bytes.Repeat([]byte{'a'}, 8192), s.state.Agent.History)
		require.Zero(t, s.state.Agent.V2.ControlHigh, "refusal cannot consume the sequence")
		s.mu.Unlock()
	}
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	receipt, err := w.AgentControl(ctx, call.id, agent.ControlMessage{ID: "accepted", Sequence: 1, Name: "remember", Data: []byte{'c'}})
	require.NoError(t, err)
	require.EqualValues(t, 1, receipt.Sequence)
	s.mu.Lock()
	before := s.state.Checkpoint.Successes
	s.mu.Unlock()
	w.paused.Store(false)
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Checkpoint.Successes >= before+3 }, 2*time.Second, 10*time.Millisecond, "transport checkpoints continue after refused and accepted history updates")
	require.NoError(t, s.ctx.Err(), "history pressure cannot terminate the call")
	tuple, err := store.ReadComponents(ctx, s.id)
	require.NoError(t, err)
	restored, _, err := assembleSessionComponents(sessionComponents(tuple.Components), tuple.Metadata.StateVersion)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{'c'}, 8192), restored.State.Agent.History)

}

func TestSplitSideEffectLaunchWaitsForForcedBarrier(t *testing.T) {
	store := sessionstore.NewMemory()
	s := componentWriterSession(t, store, true)
	launched := make(chan struct{}, 1)
	bounds := effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)
	h := &v2Host{registration: agenthost.Registration{Bounds: bounds, Tasks: map[string]agent.TaskHandler{"effect/v1": func(ctx context.Context, _ *agent.TaskRun) error {
		launched <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}}, force: make(chan struct{}, 1), emissions: make(chan taskEmission)}
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: h}
	spec := agent.TaskSpec{Step: "effect", Kind: "effect/v1", Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("request")}
	intent := taskIntent{ID: "stable-effect", Step: spec.Step, Kind: spec.Kind, Class: spec.Class, Resume: spec.Resume, Digest: taskDigest(spec), Phase: taskNever, Request: spec.Request, RequestLength: len(spec.Request)}
	h.tasks = []*localTask{{intent: intent, request: spec.Request, terminalBytes: taskTerminalBytes(intent)}}
	s.launchTasks()
	require.True(t, h.tasks[0].prepared)
	require.False(t, h.tasks[0].launched, "an accepted intent does not permit the side effect")
	select {
	case <-h.force:
	default:
		t.Fatal("side-effect intent did not request a forced checkpoint")
	}
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointIntent))
	require.EqualValues(t, 1, s.worker.metrics.forcedSuccesses.Load())
	s.launchTasks()
	require.True(t, h.tasks[0].launched)
	select {
	case <-launched:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpointed effect did not launch")
	}
	s.mu.Lock()
	require.NotNil(t, s.lastTaskLaunch)
	require.True(t, s.lastTaskLaunch.Checkpointed)
	s.mu.Unlock()
	s.stopTaskAttempts()
}
