package callharness

import (
	"context"
	"encoding/json"
	"fmt"
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
	capacity chan agent.TaskID
	hold     func() error
	batch    int
}

func (a *finalRetryOnCapacityAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.TaskMessage && e.Result.Outcome == agent.Capacity {
		if a.hold != nil {
			if err := a.hold(); err != nil {
				return agent.Effects{}, err
			}
			// Only the first source notification is needed. Transaction retries
			// must not block while the test leaves that notification unread.
			select {
			case a.capacity <- e.Result.TaskID:
			default:
			}
		} else {
			a.capacity <- e.Result.TaskID
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
func TestV2TaskCapacityRetryKeepsCall(t *testing.T) {
	for _, batch := range []int{1, 32} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				specs := make([]agent.TaskSpec, 12)
				for i := range specs {
					specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("r")}
				}
				sourceCapacity, targetCapacity := make(chan agent.TaskID, 1), make(chan agent.TaskID, 32)
				var sys *agentHarnessSystem
				reg := agenthost.Registration{Name: "capacity-retry/v1", Tasks: map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}, Factory: func(agent.FactoryArgs) agent.Agent {
					return &finalRetryOnCapacityAgent{taskHarnessAgent: taskHarnessAgent{specs: specs}, batch: batch, capacity: sourceCapacity, hold: func() error {
						// Issue one retry batch, then move with its outcomes still pending.
						// A backpressured retry retains the original Capacity outcome.
						return workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true)
					}}
				}}
				targetReg := reg
				targetReg.Factory = func(agent.FactoryArgs) agent.Agent {
					// Drain moved outcomes without recursively starting more tasks.
					return &finalRetryOnCapacityAgent{taskHarnessAgent: taskHarnessAgent{specs: specs}, capacity: targetCapacity}
				}
				// Wide admission shares the callback budget. This test checks move
				// continuity, not the default 100 ms callback deadline under -race.
				sys = startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, targetReg}, callbackBudget: 5 * time.Second})
				require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[1].LocalAddr(), true))
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				require.Eventually(t, func() bool { return len(sourceCapacity) > 0 }, 2*time.Second, time.Millisecond)
				require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
				moved, err := sys.plane.Move(ctx, call.SessionID(), "1")
				require.NoError(t, err)
				require.False(t, moved.Lost)
				// Inspect the restored mailbox before allowing the target to consume it.
				wire, err := sys.workers[1].SnapshotSession(call.SessionID())
				require.NoError(t, err)
				var pending struct {
					State struct {
						Agent struct {
							V2 struct {
								Capacity []struct {
									ID agent.TaskID `json:"i"`
								}
							}
						}
					}
				}
				require.NoError(t, json.Unmarshal(wire, &pending))
				require.NotEmpty(t, pending.State.Agent.V2.Capacity, "move must carry pending Capacity outcomes")
				require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[1].LocalAddr(), false))
				received := map[agent.TaskID]bool{}
				require.Eventually(t, func() bool {
					for len(targetCapacity) > 0 {
						received[<-targetCapacity] = true
					}
					for _, outcome := range pending.State.Agent.V2.Capacity {
						if !received[outcome.ID] {
							return false
						}
					}
					return true
				}, 3*time.Second, time.Millisecond, "target must receive every moved Capacity outcome")
				require.NoError(t, call.SendMedia(ctx, 150*time.Millisecond))
				require.Equal(t, 1, sys.workers[1].SessionCount(), "target stats: %+v", sys.workers[1].AgentStats())
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestV2TaskFinishedRetryTakeover(t *testing.T) {
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
func TestV2TaskPreparedSlots(t *testing.T) {
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
