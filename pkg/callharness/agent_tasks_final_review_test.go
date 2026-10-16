package callharness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type finalRetryOnCapacityAgent struct {
	taskHarnessAgent
	observed       *atomic.Int32
	targetObserved *atomic.Int32
	batch          int
}

func (a *finalRetryOnCapacityAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.TaskMessage && e.Result.Outcome == agent.Capacity {
		a.observed.Add(1)
		if a.targetObserved != nil {
			a.targetObserved.Add(1)
		}
		specs := make([]agent.TaskSpec, a.batch)
		for i := range specs {
			specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("r")}
		}
		return agent.Effects{StartTasks: specs}, nil
	}
	if e.Kind == agent.TaskMessage {
		return agent.Effects{}, nil
	}
	return a.taskHarnessAgent.Handle(ctx, e)
}
func TestV2TaskFinalReviewCapacityRetryKeepsCall(t *testing.T) {
	for _, batch := range []int{1, 32} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				observed := &atomic.Int32{}
				specs := make([]agent.TaskSpec, 12)
				for i := range specs {
					specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("r")}
				}
				reg := agenthost.Registration{Name: "capacity-retry/v1", Tasks: map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}, Factory: func(agent.FactoryArgs) agent.Agent {
					return &finalRetryOnCapacityAgent{taskHarnessAgent: taskHarnessAgent{specs: specs}, observed: observed, batch: batch}
				}}
				targetSeen := &atomic.Int32{}
				targetReg := reg
				targetReg.Factory = func(agent.FactoryArgs) agent.Agent {
					return &finalRetryOnCapacityAgent{taskHarnessAgent: taskHarnessAgent{specs: specs}, observed: observed, targetObserved: targetSeen, batch: batch}
				}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, targetReg}})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				require.Eventually(t, func() bool { return observed.Load() >= 12 }, 2*time.Second, time.Millisecond)
				require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
				moved, err := sys.plane.Move(ctx, call.SessionID(), "1")
				require.NoError(t, err)
				require.False(t, moved.Lost)
				require.Eventually(t, func() bool { return targetSeen.Load() > 0 || sys.workers[1].SessionCount() == 0 }, 3*time.Second, time.Millisecond)
				require.Positive(t, targetSeen.Load(), "target must receive the moved Capacity outcome")
				require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
				require.Equal(t, 1, sys.workers[1].SessionCount(), "target stats: %+v", sys.workers[1].AgentStats())
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestV2TaskFinalReviewFinishedRetryTakeover(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		keys := make(chan string, 8)
		handler := func(ctx context.Context, run *agent.TaskRun) error {
			keys <- run.Key()
			return run.Emit(ctx, run.After()+1, agent.Chunk{Data: []byte("receipt")})
		}
		regs, results := taskRegistrations([]agent.TaskSpec{{Step: "pay", Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("args")}}, [2]agent.TaskHandler{handler, handler}, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		first := <-keys
		readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final })
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		select {
		case key := <-keys:
			require.Equal(t, first, key)
		case <-ctx.Done():
			t.Fatal("finished task did not retry")
		}
		final := takeTaskFinal(t, results[1])
		require.Equal(t, agent.OK, final[len(final)-1].Outcome)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2TaskFinalReviewPreparedSlots(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		gate := &taskOutageStore{Store: store}
		gate.blocked.Store(true)
		specs := make([]agent.TaskSpec, 6)
		for i := range specs {
			specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Abandon}
		}
		handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
		regs, _ := taskRegistrations(specs, [2]agent.TaskHandler{handler, handler}, false)
		sys := startAgentHarness(t, gate, agentHarnessSettings{v2: regs, saveInterval: time.Hour})
		gate.source = sys.workers[0].LocalAddr()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return gate.failed.Load() > 0 }, time.Second, time.Millisecond)
		wire, err := sys.workers[0].SnapshotSession(call.SessionID())
		require.NoError(t, err)
		var snap taskSnapshot
		require.NoError(t, json.Unmarshal(wire, &snap))
		require.Len(t, snap.State.Agent.V2.Tasks, 6)
		started, queued := 0, 0
		for _, task := range snap.State.Agent.V2.Tasks {
			switch task.Phase {
			case "may_have_started":
				started++
			case "never_launched":
				queued++
			}
		}
		require.Equal(t, 4, started)
		require.Equal(t, 2, queued)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
