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
	close(a.entered)
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
			w, s := v2UnitSession(t, a)
			w.cfg.Agent.CallbackTimeout = time.Second
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			a.entered, a.saving, a.release, a.returned = make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			a.blockHandle = blockHandle
			t.Cleanup(func() {
				select {
				case <-a.release:
				default:
					close(a.release)
				}
			})
			taskCtx, cancel := context.WithCancel(s.ctx)
			result := agent.TaskResult{TaskID: "shutdown", Sequence: 1, Outcome: agent.OK}
			intent := taskIntent{ID: result.TaskID, Phase: taskStarted, Published: 1, Class: agent.ReadOnly}
			s.agent.v2.tasks = []*localTask{{intent: intent, results: []agent.TaskResult{result}, launched: true, attempt: 1,
				cancel: cancel, mailboxBytes: taskResultBytes(result), terminalBytes: taskTerminalBytes(intent)}}
			s.agent.latest.V2.Started = true
			s.publishTaskIntents()
			s.publishAgent()
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
