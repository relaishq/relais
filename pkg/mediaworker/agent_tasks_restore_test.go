package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/sessionstore"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Probe 1: a takeover-restored ReadOnly task whose request bytes were lost
// remains in h.tasks until pruned. Can a planned move export it, and can the
// target (or a rollback to the source) restore that exact export?
func TestTaskLostRequestExactRestore(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(fmt.Sprint("final=", final), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
			spec := agent.TaskSpec{Step: "stt", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("recording")}
			intent := taskIntent{ID: "lost", Step: spec.Step, Kind: spec.Kind, Class: spec.Class, RequestLength: len(spec.Request), Digest: taskDigest(spec), Phase: taskStarted}
			if final {
				intent.Final, intent.Terminal, intent.Phase, intent.Published, intent.Cursor = true, agent.OK, taskTerminal, 1, 1
			}
			s.agent.latest.V2.Tasks = []taskIntent{intent}
			// Crash restore (move == nil).
			require.NoError(t, s.restoreTasks(nil))
			s.publishTaskIntents(s.agent.latest.V2)
			// Planned move export from this restored session.
			move := &movePayload{Tasks: s.exportTasks()}
			t.Logf("exported task: request=%d intent.RequestLength=%d", len(move.Tasks[0].Request), s.agent.latest.V2.Tasks[0].RequestLength)
			// Target restore of the exact export.
			b := &v2UnitAgent{state: agent.State{Version: 1}}
			_, target := v2UnitSession(t, b)
			require.NoError(t, target.initAgent(false, ResumeOptions{}))
			target.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
			target.agent.latest.V2.Tasks = cloneTaskIntents(s.agent.latest.V2.Tasks)
			err := target.restoreTasks(move)
			t.Logf("exact restore error: %v", err)
			require.NoError(t, err, "exact restore of a post-takeover session refuses the move (and the rollback)")
		})
	}
}

func TestTaskCrashRestoreDoesNotReserveLostBytes(t *testing.T) {
	const length = 900 << 10
	source := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: length}, payload: make([]byte, length)}
	_, s := v2UnitSession(t, source)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	// Target app clears move-only metadata on OutcomeUnknown, as the guide says.
	target := &v2UnitAgent{}
	target.callback = func(context.Context) { target.state.HasMoveOnly, target.state.MoveOnlyLength = false, 0 }
	w, next := v2UnitSession(t, target)
	next.state = s.state
	next.state.Agent = cloneAgentState(s.state.Agent)
	// Other sessions already hold almost all of the worker's session budget.
	pressure := int64(v2MoveWorkerBytes - v2ExportScratchBytes - (100 << 10))
	require.True(t, w.reserveMoveBytes(pressure))
	defer w.moveBytes.Add(-pressure)
	err := next.initAgent(true, ResumeOptions{Kind: legacy.Takeover, SequenceMargin: 8192})
	t.Logf("crash restore with lost 900 KiB move-only data and 100 KiB free: %v", err)
	require.NoError(t, err)
}

func TestTaskEmptyRequestCrashRestore(t *testing.T) {
	for _, class := range []agent.TaskClass{agent.ReadOnly, agent.SideEffecting} {
		t.Run(fmt.Sprint(class), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
			a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "test/v1", Class: class, Resume: agent.Retry, Request: []byte{}}}}
			_, eventErr0 := (acceptedEvents{s}).submit(agent.Event{ID: "started/x", Kind: agent.Started}, eventLive)
			require.NoError(t, eventErr0)
			data, err := json.Marshal(s.state.Agent)
			require.NoError(t, err)
			var decoded agentState
			require.NoError(t, json.Unmarshal(data, &decoded))
			b := &v2UnitAgent{state: agent.State{Version: 1}}
			_, target := v2UnitSession(t, b)
			require.NoError(t, target.initAgent(false, ResumeOptions{}))
			target.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
			target.agent.latest = decoded
			err = target.restoreTasks(nil)
			t.Logf("crash restore with empty non-nil Request: %v", err)
			require.NoError(t, err)
		})
	}
}

func TestTaskDetailExactRestore(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	spec := agent.TaskSpec{Step: "llm", Kind: "test/v1", Class: agent.ReadOnly}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{spec}}
	_, eventErr1 := (acceptedEvents{s}).submit(agent.Event{ID: "started/x", Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr1)
	a.effects = agent.Effects{}
	task := s.agent.v2.tasks[0]
	task.attempt, task.launched = 1, true
	detail := "provider error:: " + strings.Repeat("超时", 60) // 16 + 360 bytes
	e := taskEmission{id: task.intent.ID, attempt: 1, ctx: context.Background(), final: true, outcome: agent.Failed, detail: detail, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.NoError(t, <-e.reply)
	t.Logf("stored detail len=%d", len(task.results[0].Detail))
	wire, err := json.Marshal(movePayload{Tasks: s.exportTasks()})
	require.NoError(t, err)
	var move movePayload
	require.NoError(t, json.Unmarshal(wire, &move))
	t.Logf("detail after JSON round trip len=%d", len(move.Tasks[0].Results[0].Detail))
	b := &v2UnitAgent{state: agent.State{Version: 1}}
	_, target := v2UnitSession(t, b)
	require.NoError(t, target.initAgent(false, ResumeOptions{}))
	target.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
	target.agent.latest.V2.Tasks = cloneTaskIntents(s.agent.latest.V2.Tasks)
	err = target.restoreTasks(&move)
	t.Logf("exact restore: %v", err)
	require.NoError(t, err)
}

func TestTaskEmitWorkerPressureBackpressures(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	w, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "stream", Class: agent.ReadOnly, Phase: taskStarted}
	task := &localTask{intent: intent, attempt: 1, terminalBytes: taskTerminalBytes(intent), launched: true}
	s.agent.v2.tasks = []*localTask{task}
	require.NoError(t, s.taskBudget(s.agent.v2.tasks, 0))
	pressure := int64(v2MoveWorkerBytes) - w.moveBytes.Load()
	require.True(t, w.reserveMoveBytes(pressure))
	defer w.moveBytes.Add(-pressure)
	e := taskEmission{id: intent.ID, attempt: 1, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: []byte("stream")}, reply: make(chan error, 1)}
	s.acceptEmission(e)
	require.ErrorIs(t, <-e.reply, agent.ErrBackpressure)
	require.Empty(t, task.results)
}
func TestTaskPreparedTasksReserveSlots(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
	for i := range 6 {
		class := agent.SideEffecting
		policy := agent.Retry
		if i == 4 {
			class = agent.ReadOnly
			policy = ""
		}
		intent := taskIntent{ID: agent.TaskID(fmt.Sprint(i)), Kind: "test/v1", Class: class, Resume: policy, Phase: taskNever}
		s.agent.v2.tasks = append(s.agent.v2.tasks, &localTask{intent: intent, terminalBytes: taskTerminalBytes(intent)})
	}
	s.launchTasks()
	require.False(t, s.agent.v2.tasks[4].launched, "prepared tasks reserve their slots")
	for i, task := range s.agent.v2.tasks {
		if i < 4 {
			require.True(t, task.prepared)
			require.Equal(t, taskStarted, task.intent.Phase)
		} else {
			require.False(t, task.prepared)
			require.Equal(t, taskNever, task.intent.Phase)
		}
	}
	s.mu.Lock()
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
	s.mu.Unlock()
	s.launchTasks()
	for i, task := range s.agent.v2.tasks {
		if i < 4 {
			require.True(t, task.launched)
		} else {
			require.False(t, task.launched)
			require.Equal(t, taskNever, task.intent.Phase)
		}
	}
}
func TestTaskExportMismatchIsDefinitiveBeforePause(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("r")}}}
	_, eventErr2 := (acceptedEvents{s}).submit(agent.Event{ID: "cause", Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr2)
	s.agent.latest.V2.Tasks[0].RequestLength++
	s.agent.queue <- agentInput{}
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(time.Second), moveOnly: make(chan agentMoveExport, 1)}
	require.True(t, s.finishAgentFlush(request))
	err := <-request.done
	require.ErrorIs(t, err, ErrExportIncompatible)
	require.NotErrorIs(t, err, ErrMoveBudget)
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	require.False(t, s.agent.paused)
	require.False(t, s.fenced.Load())
	require.Len(t, s.agent.queue, 1)
}
func TestTaskDigestNormalizesEmpty(t *testing.T) {
	spec := agent.TaskSpec{Step: "s", Kind: "test/v1", Class: agent.ReadOnly, Request: nil}
	before := taskDigest(spec)
	spec.Request = []byte{}
	require.Equal(t, before, taskDigest(spec))
	spec.Step = "other"
	require.NotEqual(t, before, taskDigest(spec))
}

type taskClockStore struct {
	sessionstore.Store
	calls atomic.Int32
}

func (s *taskClockStore) Clock(context.Context, string) (time.Time, error) {
	s.calls.Add(1)
	return time.Time{}, sessionstore.ErrTransient
}
func TestTaskForcedRetryStopsAndRespectsPause(t *testing.T) {
	store := &taskClockStore{Store: sessionstore.NewMemory()}
	reg := agenthost.Registration{Name: "retry/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &v2UnitAgent{state: agent.State{Version: 1}} }}
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, Relay: &RelayConfig{Owners: store, Addr: netip.MustParseAddrPort("127.0.0.1:12345"), PublicAddr: netip.MustParseAddrPort("127.0.0.1:12346")}})
	require.NoError(t, err)
	defer w.Close()
	s := sessionFromState(w, sessionState{Version: 7, ID: "retry-unit"})
	defer s.close()
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "waiting", Class: agent.SideEffecting, Resume: agent.Retry, Phase: taskStarted}
	s.agent.latest.V2.Tasks = []taskIntent{intent}
	s.publishAgent()
	w.paused.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); s.taskCheckpointLoop() }()
	defer func() { s.cancel(); <-done }()
	s.agent.v2.force <- struct{}{}
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, store.calls.Load(), "paused worker must not force writes")
	w.paused.Store(false)
	require.Eventually(t, func() bool { return store.calls.Load() > 0 }, time.Second, time.Millisecond)
	s.mu.Lock()
	s.state.Agent.V2.Tasks[0].Final = true
	s.mu.Unlock()
	before := store.calls.Load()
	time.Sleep(350 * time.Millisecond)
	require.Equal(t, before, store.calls.Load(), "no prepared intent remains, so failed writes must not re-arm")
}
