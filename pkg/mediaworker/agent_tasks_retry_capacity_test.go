package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// A Capacity result that starts another refused task must yield to the actor.
type retryCapacityAgent struct {
	v2UnitAgent
	capacity int
}

func (a *retryCapacityAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.TaskMessage && e.Result.Outcome == agent.Capacity {
		a.capacity++
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "retry", Kind: "test/v1", Class: agent.ReadOnly, Request: make([]byte, 2<<20)}}}, nil
	}
	return a.v2UnitAgent.Handle(ctx, e)
}

func TestTaskExportIncompatibleHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerAPIError(w, errors.Join(ErrExportIncompatible, agent.ErrIncompatibleAgent))
	}))
	defer server.Close()
	err := privateapi.Do(context.Background(), server.Client(), server.URL, "POST", "/export", map[string]string{}, nil, RemoteErrors)
	require.ErrorIs(t, err, ErrExportIncompatible)
	require.NotErrorIs(t, err, ErrMoveBudget)
}

func TestTaskCapacityAbandonMove(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}
	for i := range 12 {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Abandon, Request: []byte("args")})
	}
	_, eventErr2 := (acceptedEvents{s}).submit(agent.Event{ID: "started", Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr2)
	s.launchTasks()
	s.mu.Lock()
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent}))
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

func TestTaskCapacityYields(t *testing.T) {
	a := &retryCapacityAgent{v2UnitAgent: v2UnitAgent{state: agent.State{Version: 1}}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, map[string]agent.TaskHandler{"test/v1": func(context.Context, *agent.TaskRun) error { return nil }}))
	require.NoError(t, s.initAgent(false, ResumeOptions{}))

	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "first", Kind: "test/v1", Class: agent.ReadOnly, Request: make([]byte, 2<<20)}}}
	_, eventErr1 := (acceptedEvents{s}).submit(agent.Event{ID: "started", Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr1)
	require.LessOrEqual(t, a.capacity, 1)
	for range 12 {
		before := a.capacity
		_, err := s.deliverTaskResult()
		require.NoError(t, err)
		require.Equal(t, before+1, a.capacity)
	}
}

// Probe 7: SideEffecting Retry task finished (chunk + OK terminal) but nothing
// consumed; crash restore from the checkpoint holding the Final intent.
func TestTaskSideEffectRetryAfterUnconsumedFinal(t *testing.T) {
	started := make(chan *agent.TaskRun, 1)
	finish := make(chan struct{})
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	handlers := map[string]agent.TaskHandler{"pay/v1": func(ctx context.Context, run *agent.TaskRun) error {
		started <- run
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finish:
			return nil
		}
	}}
	_, source := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, handlers))
	require.NoError(t, source.initAgent(false, ResumeOptions{}))
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "pay", Kind: "pay/v1", Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("args")}}}
	_, err := (acceptedEvents{source}).submit(agent.Event{Kind: agent.Started, ID: "started/x"}, eventLive)
	require.NoError(t, err)
	a.effects = agent.Effects{}
	source.launchTasks()
	require.NoError(t, source.checkpoint.restore(checkpointRestore{pair: &source.state.Agent}))
	source.launchTasks()
	t.Cleanup(source.stopTaskAttempts)
	run := <-started
	done := make(chan error, 1)
	go func() { done <- run.Emit(context.Background(), 1, agent.Chunk{Data: []byte("receipt")}) }()
	source.acceptEmission(<-source.agent.v2.emissions)
	require.NoError(t, <-done)
	close(finish)
	for !source.state.Agent.V2.Tasks[0].Final {
		source.acceptEmission(<-source.agent.v2.emissions)
	}
	require.Equal(t, []byte("args"), source.state.Agent.V2.Tasks[0].Request, "unconsumed terminal intent must retain retry arguments")
	raw, err := json.Marshal(source.state)
	require.NoError(t, err)
	var state sessionState
	require.NoError(t, json.Unmarshal(raw, &state))
	b := &v2UnitAgent{state: agent.State{Version: 1}}
	retryStarted := make(chan *agent.TaskRun, 1)
	retryHandlers := map[string]agent.TaskHandler{"pay/v1": func(_ context.Context, run *agent.TaskRun) error { retryStarted <- run; return nil }}
	_, target := v2UnitSessionFromState(t, b, state, v2UnitRegistration(b, agent.Limits{}, retryHandlers))
	require.NoError(t, target.initAgent(true, ResumeOptions{}))
	restored := target.state.Agent.V2.Tasks[0]
	require.Equal(t, taskNever, restored.Phase)
	require.False(t, restored.Final)
	require.Equal(t, run.ID(), restored.ID)
	move, err := target.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte("args"), move.tasks[0].Request)
	target.launchTasks()
	require.NoError(t, target.checkpoint.restore(checkpointRestore{pair: &target.state.Agent}))
	target.launchTasks()
	t.Cleanup(target.stopTaskAttempts)
	retried := <-retryStarted
	require.Equal(t, run.ID(), retried.ID())
	require.Equal(t, []byte("args"), retried.Spec().Request)
	for !target.state.Agent.V2.Tasks[0].Final {
		target.acceptEmission(<-target.agent.v2.emissions)
	}
	require.Equal(t, []byte("args"), target.state.Agent.V2.Tasks[0].Request)
	delivered, err := target.deliverTaskResult()
	require.NoError(t, err)
	require.True(t, delivered)
	move, err = target.saveV2MoveOnly(context.Background())
	require.NoError(t, err)
	require.Empty(t, move.tasks[0].Request)
	require.Empty(t, target.state.Agent.V2.Tasks[0].Request)
}
