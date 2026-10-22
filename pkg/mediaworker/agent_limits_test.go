package mediaworker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestV2OversizedStateRefusesWithoutEndingCall(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	a.callback = func(context.Context) { a.state.Bytes = make([]byte, 1025) }
	input := legacy.Input{Progress: legacy.Progress{SSRC: 42, Index: 7}}
	_, eventErr0 := (acceptedEvents{s}).submit(agent.Event{ID: "audio/42/7", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr0)
	require.Equal(t, "accepted", string(s.state.Agent.State.Bytes))
	require.EqualValues(t, 1, s.state.Agent.Progress.Consumed)
	require.Len(t, s.state.Agent.V2.Capacity, 1)
	require.Equal(t, agent.Capacity, s.state.Agent.V2.Capacity[0].result().Outcome)
	require.NoError(t, s.ctx.Err())
	a.callback = nil
	delivered, err := s.deliverTaskResult()
	require.NoError(t, err)
	require.False(t, delivered)
	require.Empty(t, s.state.Agent.V2.Capacity)
	_, eventErr1 := (acceptedEvents{s}).submit(agent.Event{ID: "audio/42/8", Kind: agent.AudioInput, Input: &input}, eventLive)
	require.NoError(t, eventErr1)
	require.EqualValues(t, 2, s.state.Agent.Progress.Consumed)
}

func TestV2EmitFirstImmediateThenCoalescesTerminal(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "emit", Class: agent.ReadOnly, Phase: taskStarted}
	task := &localTask{intent: intent, attempt: 1, launched: true, terminalBytes: taskTerminalBytes(intent)}
	s.agent.v2.tasks = []*localTask{task}
	emit := func(seq uint64, final bool) error {
		e := taskEmission{id: "emit", attempt: 1, ctx: context.Background(), sequence: seq, final: final, outcome: agent.OK, chunk: agent.Chunk{Data: []byte("chunk")}, reply: make(chan error, 1)}
		s.acceptEmission(e)
		return <-e.reply
	}
	begin := time.Now()
	require.NoError(t, emit(1, false))
	require.Less(t, time.Since(begin), 50*time.Millisecond)
	require.ErrorIs(t, emit(2, false), agent.ErrBackpressure)
	require.ErrorIs(t, emit(0, true), agent.ErrBackpressure)
	require.Len(t, task.results, 1)
	require.Eventually(t, func() bool { return emit(2, false) == nil }, time.Second, time.Millisecond)
	require.GreaterOrEqual(t, task.lastEmission.Sub(begin), 100*time.Millisecond)
	require.ErrorIs(t, emit(0, true), agent.ErrBackpressure)
	require.Eventually(t, func() bool { return emit(0, true) == nil }, time.Second, time.Millisecond)
	require.Len(t, task.results, 3)
	require.True(t, task.results[2].Final)
}

type rejectedSnapshotStore struct {
	sessionstore.Store
	fail atomic.Bool
}

func (s *rejectedSnapshotStore) PutState(ctx context.Context, l sessionstore.Lease, data []byte) error {
	if s.fail.Load() {
		return errors.New("forced store outage")
	}
	return s.Store.PutState(ctx, l, data)
}

func TestV2FailedForcedWriteDoesNotDebit(t *testing.T) {
	reg := agenthost.Registration{Name: "bytes/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }}
	failing := &rejectedSnapshotStore{Store: sessionstore.NewMemory()}
	r, err := relay.New(relay.Config{Owners: failing})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: failing, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	r.AddWorker(w.LocalAddr())
	call, _ := dialDTLSCaller(t, w)
	w.paused.Store(true)
	s := w.session(call.id)
	require.Eventually(t, func() bool { return checkpointKnown(s) }, time.Second, time.Millisecond, "initial handshake checkpoint must finish before injecting the forced-write failure")

	s.checkpoint.wait()
	restoreCheckpointQuota(t, s, float64(s.snapshotLimits().BurstBytes), s.taskNow())
	failing.fail.Store(true)
	err = s.checkpoint.persist(context.Background(), checkpointRequired)
	require.ErrorContains(t, err, "forced store outage")
	require.NotErrorIs(t, err, agent.ErrCapacity)
	require.InDelta(t, float64(s.snapshotLimits().BurstBytes), float64(checkpointQuota(t, s)), 1)
	// The same forced intent fits immediately after the failure; an outage has
	// neither consumed capacity nor granted durable launch permission.
	failing.fail.Store(false)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointIntent))
	data, err := failing.GetState(context.Background(), call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	balance, err := snap.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.EqualValues(t, w.cfg.SnapshotLimits.BurstBytes-len(data)-snapshotEnvelopeBytes, balance)
	require.Less(t, float64(checkpointQuota(t, s)), float64(w.cfg.SnapshotLimits.BurstBytes))
}

type snapshotBudgetAgent struct{}

func (*snapshotBudgetAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (*snapshotBudgetAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1}, nil
}
func (*snapshotBudgetAgent) Restore(context.Context, agent.State) error { return nil }

func TestV2AudioEffectsValidateCopiedTasks(t *testing.T) {
	packets := [][]byte{{0xf8, 0xff, 0xfe}}
	rig, runs := startAcceptedSpeech(t, agenthost.Bounds{}, packets)
	original := rig.app.handle
	mutate := make(chan struct{})
	mutated := make(chan struct{})
	release := sync.OnceFunc(func() { close(mutate) })
	t.Cleanup(release)
	rig.app.handle = func(ctx context.Context, event agent.Event) (agent.Effects, error) {
		effects, err := original(ctx, event)
		if event.Kind == agent.TaskMessage {
			go func() {
				<-mutate
				// An agent may retain its callback values after the transaction returns.
				event.Result.TaskID = "changed"
				event.Result.Turn = 2
				event.Result.Chunk.Audio.Packets[0][1] = 0
				effects.Audio[0].Turn = 2
				effects.Audio[0].Source.TaskID = "changed"
				close(mutated)
			}()
		}
		return effects, err
	}
	delivered, err := rig.s.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	before, err := rig.s.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	release()
	<-mutated
	after, err := rig.s.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	saved, err := decodeSnapshot(after)
	require.NoError(t, err)
	prior, err := decodeSnapshot(before)
	require.NoError(t, err)
	require.Equal(t, prior.State.Agent.V2.Tasks, saved.State.Agent.V2.Tasks, "late callback mutation cannot change accepted tasks")
	require.Equal(t, prior.State.Agent.V2.Playback, saved.State.Agent.V2.Playback, "late callback mutation cannot change accepted playback")
	require.Len(t, saved.State.Agent.V2.Tasks, 1)
	require.Equal(t, runs[0].ID(), saved.State.Agent.V2.Tasks[0].ID)
	require.EqualValues(t, 1, saved.State.Agent.V2.Tasks[0].Turn)
	move, err := rig.s.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Equal(t, []agent.Audio{{Turn: 1, Source: agent.AudioRef{TaskID: runs[0].ID(), Sequence: 1}}}, move.audio)
	require.Len(t, move.tasks, 1)
	require.Equal(t, [][]byte{{0xf8, 0xff, 0xfe}}, move.tasks[0].Audio[0].Chunk.Audio.Packets, "late callback mutation cannot change retained task payload")
}

func TestV2EmitRestoreDoesNotGrantImmediatePublication(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, source := v2UnitSession(t, a)
	require.NoError(t, source.initAgent(false, ResumeOptions{}))
	handler := func(context.Context, *agent.TaskRun) error { return nil }
	source.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"stream/v1": handler}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "stream", Kind: "stream/v1", Class: agent.ReadOnly, Resume: agent.Retry}}}
	_, eventErr2 := (acceptedEvents{source}).submit(agent.Event{ID: "started", Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr2)
	task := source.agent.v2.tasks[0]
	task.launched, task.intent.Phase = true, taskStarted
	first := taskEmission{id: task.intent.ID, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: []byte("one")}, reply: make(chan error, 1)}
	source.acceptEmission(first)
	require.NoError(t, <-first.reply)
	_, target := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	require.NoError(t, target.initAgent(false, ResumeOptions{}))
	target.agent.v2.registration.Tasks = source.agent.v2.registration.Tasks
	target.agent.latest = cloneAgentState(source.agent.latest)
	require.NoError(t, target.restoreTasks(&movePayload{Tasks: source.exportTasks()}))
	next := taskEmission{id: task.intent.ID, ctx: context.Background(), sequence: 2, chunk: agent.Chunk{Data: []byte("two")}, reply: make(chan error, 1)}
	target.acceptEmission(next)
	require.ErrorIs(t, <-next.reply, agent.ErrBackpressure)
	require.Eventually(t, func() bool { target.acceptEmission(next); return <-next.reply == nil }, time.Second, time.Millisecond)
}
