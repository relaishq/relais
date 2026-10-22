package mediaworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/taskrun"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestTaskIDLengthPrefixedAndStable(t *testing.T) {
	id := deriveTaskID("session", "agent/v1", "audio/42/2", "stt")
	require.Equal(t, id, deriveTaskID("session", "agent/v1", "audio/42/2", "stt"))
	for _, other := range []agent.TaskID{deriveTaskID("session2", "agent/v1", "audio/42/2", "stt"), deriveTaskID("session", "agent/v2", "audio/42/2", "stt"), deriveTaskID("session", "agent/v1", "audio/42/3", "stt"), deriveTaskID("session", "agent/v1", "audio/42/2", "tts"), deriveTaskID("a", "bc", "d", "e"), deriveTaskID("ab", "c", "d", "e")} {
		require.NotEqual(t, id, other)
	}
	require.NotEqual(t, deriveTaskID("a", "bc", "d", "e"), deriveTaskID("ab", "c", "d", "e"))
}
func TestTaskRunContract(t *testing.T) {
	request := []byte("owned")
	var got agent.Chunk
	run := taskrun.New(agent.TaskSpec{Request: request}, "stable", 7, func(_ context.Context, seq uint64, c agent.Chunk) error {
		require.EqualValues(t, 8, seq)
		got = c
		return nil
	})
	request[0] = 'X'
	spec := run.Spec()
	require.Equal(t, "owned", string(spec.Request))
	spec.Request[0] = 'Y'
	require.Equal(t, "owned", string(run.Spec().Request))
	require.Equal(t, agent.TaskID("stable"), run.ID())
	require.Equal(t, "stable", run.Key())
	require.EqualValues(t, 7, run.After())
	require.NoError(t, run.Emit(context.Background(), 8, agent.Chunk{Data: []byte("data")}))
	require.Equal(t, "data", string(got.Data))
	for _, o := range []agent.Outcome{agent.Failed, agent.Timeout, agent.Canceled, agent.OutcomeUnknown, agent.Capacity} {
		var failure *taskrun.Failure
		require.ErrorAs(t, run.Fail(o, "detail"), &failure)
		require.Equal(t, o, failure.Outcome)
	}
	var failure *taskrun.Failure
	require.ErrorAs(t, run.Fail("invalid", "bad"), &failure)
	require.Equal(t, agent.Failed, failure.Outcome)
	require.True(t, taskrun.ValidOutcome(agent.OK))
	require.False(t, taskrun.ValidOutcome("invalid"))
}
func TestTaskDeadlineAtLaunchAndFifthQueues(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	h := s.agent.v2
	h.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
	for i := range 5 {
		intent := taskIntent{ID: agent.TaskID(string(rune('a' + i))), Kind: "wait/v1", Class: agent.ReadOnly, Phase: taskNever}
		h.tasks = append(h.tasks, &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent)})
	}
	s.launchTasks()
	for _, task := range h.tasks[:4] {
		require.True(t, task.launched)
	}
	require.False(t, h.tasks[4].launched)
	require.Equal(t, taskNever, h.tasks[4].intent.Phase)
	require.Empty(t, h.tasks[4].results)
	h.tasks[4].intent.Deadline = time.Now().Add(-time.Second)
	s.launchTasks()
	require.Equal(t, agent.Timeout, h.tasks[4].results[0].Outcome)
	require.False(t, h.tasks[4].launched)
	s.stopTaskAttempts()
}
func TestTaskCancellationFencesLateResult(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "id", Class: agent.ReadOnly, Phase: taskStarted}
	ctx, cancel := context.WithCancel(s.ctx)
	task := &localTask{intent: intent, attempt: 1, cancel: cancel, launched: true}
	s.agent.v2.tasks = []*localTask{task}
	s.cancelTask(task)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	e := taskEmission{id: "id", attempt: 1, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: []byte("late")}, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, context.Canceled)
	require.Len(t, task.results, 1)
	require.Equal(t, agent.Canceled, task.results[0].Outcome)
}
func TestTaskEffectsAtomicAndConflict(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	spec := agent.TaskSpec{Step: "one", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("original")}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{spec}}
	event := agent.Event{ID: "cause", Kind: agent.Started}
	require.NoError(t, s.handleV2Event(event))
	require.Len(t, s.agent.v2.tasks, 1)
	id := s.agent.v2.tasks[0].intent.ID
	require.NoError(t, s.handleV2Event(event))
	require.Len(t, s.agent.v2.tasks, 1)
	require.Equal(t, id, s.agent.v2.tasks[0].intent.ID)
	a.effects.StartTasks[0].Request = []byte("conflict")
	require.ErrorIs(t, s.handleV2Event(event), agent.ErrIncompatibleAgent)
	require.Equal(t, "original", string(s.agent.v2.tasks[0].request))
}
func TestTaskMailboxBoundAndOwnedPayload(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "id", Class: agent.ReadOnly, Phase: taskStarted}
	task := &localTask{intent: intent, attempt: 1, launched: true, terminalBytes: taskTerminalBytes(intent)}
	s.agent.v2.tasks = []*localTask{task}
	payload := []byte("accepted")
	e := taskEmission{id: "id", attempt: 1, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: payload}, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.NoError(t, <-e.reply)
	payload[0] = 'X'
	require.Equal(t, "accepted", string(task.results[0].Chunk.Data))
	s.agent.v2.registration.Bounds.MaxMailboxBytes = int(s.moveReserved)
	e.sequence = 2
	e.chunk.Data = []byte("accepted")
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, agent.ErrBackpressure)
	require.Len(t, task.results, 1)
	s.terminalTask(task, agent.Failed, "reserved")
	require.Equal(t, agent.Failed, task.results[1].Outcome)
	require.False(t, errors.Is(agent.ErrBackpressure, agent.ErrCapacity))
}

func TestTaskHandlerTypedTerminals(t *testing.T) {
	for _, outcome := range []agent.Outcome{agent.OK, agent.Failed, agent.Timeout, agent.Canceled, agent.OutcomeUnknown, agent.Capacity} {
		t.Run(string(outcome), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			h := s.agent.v2
			h.registration.Tasks = map[string]agent.TaskHandler{"result/v1": func(_ context.Context, r *agent.TaskRun) error {
				switch outcome {
				case agent.OK:
					return nil
				case agent.Failed:
					return errors.New("provider failed")
				case agent.Timeout:
					return context.DeadlineExceeded
				case agent.Canceled:
					return context.Canceled
				default:
					return r.Fail(outcome, "typed")
				}
			}}
			intent := taskIntent{ID: "typed", Kind: "result/v1", Class: agent.ReadOnly, Phase: taskNever}
			task := &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent)}
			h.tasks = []*localTask{task}
			s.launchTasks()
			select {
			case e := <-h.emissions:
				s.acceptEmission(e)
			case <-time.After(time.Second):
				t.Fatal("missing terminal")
			}
			require.Len(t, task.results, 1)
			require.Equal(t, outcome, task.results[0].Outcome)
			require.True(t, task.results[0].Final)
		})
	}
}
func TestTaskReceiptsHaveNoLifetimeCap(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	for i := range 160 {
		intent := taskIntent{ID: agent.TaskID(string(rune(i + 1))), Class: agent.ReadOnly, Phase: taskTerminal, Published: 1, Cursor: 1, Final: true}
		task := &localTask{intent: intent}
		s.agent.v2.tasks = append(s.agent.v2.tasks, task)
		s.publishTaskIntents()
		s.publishAgent()
		s.mu.Lock()
		require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
		s.mu.Unlock()
		s.pruneTasks()
		require.Empty(t, s.agent.v2.tasks)
	}
	// Input receipts survive until the actual duplicate window passes the cause.
	intent := taskIntent{ID: "input", Class: agent.ReadOnly, Phase: taskTerminal, Published: 1, Cursor: 1, Final: true, InputCause: &taskCause{SSRC: 42, Index: 10}}
	s.agent.v2.tasks = []*localTask{{intent: intent}}
	s.publishTaskIntents()
	s.publishAgent()
	s.mu.Lock()
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
	s.state.Agent.Progress.SSRC = 42
	s.state.Agent.Progress.Index = 10
	s.state.SRTP.Inbound = map[uint32]uint64{42: 73}
	s.mu.Unlock()
	s.pruneTasks()
	require.Len(t, s.agent.v2.tasks, 1)
	s.mu.Lock()
	s.state.SRTP.Inbound[42] = 74
	s.mu.Unlock()
	s.pruneTasks()
	require.Empty(t, s.agent.v2.tasks)
}

func TestTaskTakeoverPreservesTerminalAndCancellation(t *testing.T) {
	for _, outcome := range []agent.Outcome{agent.OK, agent.Failed, agent.Timeout, agent.Canceled, agent.OutcomeUnknown, agent.Capacity} {
		t.Run(string(outcome), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { t.Error("terminal task relaunched"); return nil }}
			phase := taskTerminal
			if outcome == agent.Canceled {
				phase = taskCanceled
			}
			s.agent.latest.V2.Tasks = []taskIntent{{ID: "terminal", Kind: "test/v1", Class: agent.ReadOnly, Phase: phase, Published: 1, Final: true, Terminal: outcome, Digest: taskDigest(agent.TaskSpec{Kind: "test/v1", Class: agent.ReadOnly})}}
			require.NoError(t, s.restoreTasks(nil))
			require.Len(t, s.agent.v2.tasks, 1)
			task := s.agent.v2.tasks[0]
			require.Len(t, task.results, 1)
			require.Equal(t, outcome, task.results[0].Outcome)
			s.launchTasks()
			require.False(t, task.launched)
		})
	}
}

func TestTaskInterruptedExportResumesLocally(t *testing.T) {
	for _, policy := range []agent.ResumePolicy{agent.Retry, agent.Abandon} {
		t.Run(string(policy), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			launches := make(chan agent.TaskID, 4)
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(ctx context.Context, r *agent.TaskRun) error { launches <- r.ID(); <-ctx.Done(); return ctx.Err() }}
			intent := taskIntent{ID: "same", Kind: "wait/v1", Class: agent.ReadOnly, Resume: policy, Phase: taskNever}
			task := &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent)}
			s.agent.v2.tasks = []*localTask{task}
			s.launchTasks()
			select {
			case <-launches:
			case <-time.After(time.Second):
				t.Fatal("missing first launch")
			}
			s.stopTaskAttempts()
			s.launchTasks()
			if policy == agent.Retry {
				select {
				case id := <-launches:
					require.Equal(t, agent.TaskID("same"), id)
				case <-time.After(time.Second):
					t.Fatal("failed export stranded task")
				}
			} else {
				require.False(t, task.launched)
				require.Equal(t, agent.OutcomeUnknown, task.results[0].Outcome)
				require.Empty(t, launches)
			}
		})
	}
}

func TestTaskTakeoverRequiresReproduciblePrefix(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	spec := agent.TaskSpec{Step: "prefix", Kind: "test/v1", Class: agent.ReadOnly}
	s.agent.latest.V2.Tasks = []taskIntent{{ID: "prefix", Step: spec.Step, Kind: spec.Kind, Class: spec.Class, Digest: taskDigest(spec), Phase: taskStarted, Cursor: 1, Published: 1}}
	require.NoError(t, s.restoreTasks(nil))
	require.Len(t, s.agent.v2.tasks[0].results, 1, "a task without explicit resumable-prefix policy cannot refill existing references")
	require.Equal(t, agent.OutcomeUnknown, s.agent.v2.tasks[0].results[0].Outcome)
}
