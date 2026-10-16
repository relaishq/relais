package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/relaishq/relais/internal/privateapi"
	"net/http"
	"net/http/httptest"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// Probe 7: SideEffecting Retry task finished (chunk + OK terminal) but nothing
// consumed; crash restore from the checkpoint holding the Final intent.
func TestTaskFinalReviewSideEffectRetryAfterUnconsumedFinal(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"pay/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "pay", Kind: "pay/v1", Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("args")}}}
	require.NoError(t, s.handleV2Event(agent.Event{ID: "started/x", Kind: agent.Started}))
	task := s.agent.v2.tasks[0]
	task.attempt, task.launched, task.intent.Phase = 1, true, taskStarted
	for _, e := range []taskEmission{
		{id: task.intent.ID, attempt: 1, ctx: context.Background(), sequence: 1, chunk: agent.Chunk{Data: []byte("receipt")}, reply: make(chan error, 1)},
		{id: task.intent.ID, attempt: 1, ctx: context.Background(), final: true, outcome: agent.OK, reply: make(chan error, 1)},
	} {
		s.acceptEmission(e)
		require.NoError(t, <-e.reply)
	}
	data, err := json.Marshal(s.state.Agent)
	require.NoError(t, err)
	var decoded agentState
	require.NoError(t, json.Unmarshal(data, &decoded))
	t.Logf("checkpointed intent: final=%v published=%d cursor=%d requestLen=%d request=%q", decoded.V2.Tasks[0].Final, decoded.V2.Tasks[0].Published, decoded.V2.Tasks[0].Cursor, decoded.V2.Tasks[0].RequestLength, decoded.V2.Tasks[0].Request)
	b := &v2UnitAgent{state: agent.State{Version: 1}}
	_, target := v2UnitSession(t, b)
	require.NoError(t, target.initAgent(false, ResumeOptions{}))
	target.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
	target.agent.latest = decoded
	require.NoError(t, target.restoreTasks(nil))
	rt := target.agent.v2.tasks[0]
	outcome := agent.Outcome("")
	if len(rt.results) > 0 {
		outcome = rt.results[len(rt.results)-1].Outcome
	}
	t.Logf("after crash restore: phase=%s final=%v results=%d last=%s", rt.intent.Phase, rt.intent.Final, len(rt.results), outcome)
	require.Equal(t, taskNever, rt.intent.Phase)
	require.False(t, rt.intent.Final)
	require.Equal(t, task.intent.ID, rt.intent.ID)
	require.Equal(t, []byte("args"), rt.request)
	target.terminalTask(rt, agent.OK, "")
	require.Equal(t, []byte("args"), rt.intent.Request)
	_, err = target.deliverTaskResult()
	require.NoError(t, err)
	rt = target.agent.v2.tasks[0] // result consumption commits an owned task node
	require.Empty(t, rt.request)
	require.Empty(t, rt.intent.Request)
}

// A Capacity result that starts another refused task must yield to the actor.
type finalReviewRetryCapacityAgent struct {
	v2UnitAgent
	capacity int
}

func (a *finalReviewRetryCapacityAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.TaskMessage && e.Result.Outcome == agent.Capacity {
		a.capacity++
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "retry", Kind: "test/v1", Class: agent.ReadOnly, Request: make([]byte, 2<<20)}}}, nil
	}
	return a.v2UnitAgent.Handle(ctx, e)
}
func TestTaskFinalReviewCapacityYields(t *testing.T) {
	a := &finalReviewRetryCapacityAgent{v2UnitAgent: v2UnitAgent{state: agent.State{Version: 1}}}
	_, s := v2UnitSession(t, &a.v2UnitAgent)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.instance = a
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "first", Kind: "test/v1", Class: agent.ReadOnly, Request: make([]byte, 2<<20)}}}
	require.NoError(t, s.handleV2Event(agent.Event{ID: "started", Kind: agent.Started}))
	require.LessOrEqual(t, a.capacity, 1)
	for range 12 {
		before := a.capacity
		_, err := s.deliverTaskResult()
		require.NoError(t, err)
		require.Equal(t, before+1, a.capacity)
	}
}

func TestTaskFinalReviewExportIncompatibleHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerAPIError(w, errors.Join(ErrExportIncompatible, agent.ErrIncompatibleAgent))
	}))
	defer server.Close()
	err := privateapi.Do(context.Background(), server.Client(), server.URL, "POST", "/export", map[string]string{}, nil, RemoteErrors)
	require.ErrorIs(t, err, ErrExportIncompatible)
	require.NotErrorIs(t, err, ErrMoveBudget)
}

func TestTaskFinalReviewCapacityAbandonMove(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
	for i := range 12 {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Abandon, Request: []byte("args")})
	}
	require.NoError(t, s.handleV2Event(agent.Event{ID: "started", Kind: agent.Started}))
	s.launchTasks()
	s.mu.Lock()
	s.durableAgent = cloneAgentState(s.state.Agent)
	s.mu.Unlock()
	s.launchTasks()
	move := &movePayload{Tasks: s.exportTasks()}
	sourceErr := s.validateTaskMoves(s.agent.latest.V2.Tasks, move)
	_, target := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	require.NoError(t, target.initAgent(false, ResumeOptions{}))
	target.agent.v2.registration.Tasks = s.agent.v2.registration.Tasks
	target.agent.latest = cloneAgentState(s.agent.latest)
	if sourceErr == nil {
		require.NoError(t, target.restoreTasks(move), "source accepted an export the target cannot restore")
	}
}
