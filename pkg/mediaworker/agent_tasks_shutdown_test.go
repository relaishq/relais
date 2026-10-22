package mediaworker

import (
	"context"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

type shutdownTaskAgent struct {
	v2UnitAgent
	entered     chan struct{}
	saving      chan struct{}
	release     chan struct{}
	returned    chan struct{}
	blockHandle bool
}

func (a *shutdownTaskAgent) Handle(ctx context.Context, event agent.Event) (agent.Effects, error) {
	if a.entered != nil {
		close(a.entered)
	}
	if a.blockHandle {
		<-a.release // Deliberately ignore cancellation until the test releases us.
		defer close(a.returned)
	}
	return a.v2UnitAgent.Handle(ctx, event)
}

func (a *shutdownTaskAgent) Save(ctx context.Context) (agent.State, error) {
	if a.entered != nil && !a.blockHandle {
		close(a.saving)
		<-a.release // Keep the host closure alive after its task-state copy.
		defer close(a.returned)
		return agent.State{}, ctx.Err()
	}
	return a.v2UnitAgent.Save(ctx)
}

func TestV2TaskShutdownFencesInFlightCallback(t *testing.T) {
	for _, blockHandle := range []bool{false, true} {
		name := "save"
		if blockHandle {
			name = "handle"
		}
		t.Run(name, func(t *testing.T) {
			a := &shutdownTaskAgent{v2UnitAgent: v2UnitAgent{state: agent.State{Version: 1}}}
			started := make(chan struct {
				run *agent.TaskRun
				ctx context.Context
			}, 1)
			handlers := map[string]agent.TaskHandler{"shutdown/v1": func(ctx context.Context, run *agent.TaskRun) error {
				started <- struct {
					run *agent.TaskRun
					ctx context.Context
				}{run, ctx}
				<-ctx.Done()
				return ctx.Err()
			}}
			w, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, handlers))
			w.cfg.Agent.CallbackTimeout = time.Second
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "shutdown", Kind: "shutdown/v1", Class: agent.ReadOnly}}}
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "start"}, eventLive)
			require.NoError(t, err)
			a.effects = agent.Effects{}
			s.launchTasks()
			task := <-started
			done := make(chan error, 1)
			go func() { done <- task.run.Emit(context.Background(), 1, agent.Chunk{}) }()
			s.acceptEmission(<-s.agent.v2.emissions)
			require.NoError(t, <-done)

			a.entered, a.saving, a.release, a.returned = make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			a.blockHandle = blockHandle
			t.Cleanup(func() {
				select {
				case <-a.release:
				default:
					close(a.release)
				}
			})
			taskCtx := task.ctx
			before := cloneAgentState(s.state.Agent)
			close(s.agent.v2.ready)
			loopDone := make(chan struct{})
			w.running.Add(1)
			go func() {
				defer w.running.Done()
				defer close(loopDone)
				s.agentLoop()
			}()
			select {
			case <-a.entered:
			case <-time.After(time.Second):
				t.Fatal("task result callback did not start")
			}
			// Do not synchronize with Save before closing: doing so would order
			// its preceding task copy before shutdown and hide the reported race.
			time.Sleep(20 * time.Millisecond)
			s.close()
			select {
			case <-loopDone:
			case <-time.After(time.Second):
				t.Fatal("shutdown waited for a callback that ignores cancellation")
			}
			if !blockHandle {
				select {
				case <-a.saving:
				default:
					t.Fatal("callback did not reach the task-state staging interleaving")
				}
			}
			require.ErrorIs(t, taskCtx.Err(), context.Canceled)
			require.Equal(t, before, s.state.Agent, "shutdown must not publish an in-flight callback")
			close(a.release)
			select {
			case <-a.returned:
			case <-time.After(time.Second):
				t.Fatal("released callback did not return")
			}
			require.Equal(t, before, s.state.Agent, "late callback results remain fenced")
		})
	}
}
