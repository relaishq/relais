package callharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type taskHarnessState struct {
	Results []agent.TaskResult
}
type taskHarnessAgent struct {
	state         taskHarnessState
	specs         []agent.TaskSpec
	results       chan agent.TaskResult
	cancelOnChunk bool
}

func (a *taskHarnessAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	switch e.Kind {
	case agent.Started:
		return agent.Effects{StartTasks: a.specs}, nil
	case agent.TaskMessage:
		a.state.Results = append(a.state.Results, *e.Result)
		a.results <- *e.Result
		if a.cancelOnChunk && !e.Result.Final {
			return agent.Effects{CancelTasks: []agent.TaskID{e.Result.TaskID}}, nil
		}
	}
	return agent.Effects{}, nil
}
func (a *taskHarnessAgent) Save(context.Context) (agent.State, error) {
	// Application history records identities/outcomes, never result payloads.
	records := append([]agent.TaskResult(nil), a.state.Results...)
	for i := range records {
		records[i].Chunk = agent.Chunk{}
	}
	data, err := json.Marshal(taskHarnessState{Results: records})
	return agent.State{Version: 1, Bytes: data}, err
}
func (a *taskHarnessAgent) Restore(_ context.Context, state agent.State) error {
	return json.Unmarshal(state.Bytes, &a.state)
}

type taskLaunch struct {
	id      agent.TaskID
	key     string
	after   uint64
	request []byte
}

func taskRegistrations(specs []agent.TaskSpec, handlers [2]agent.TaskHandler, cancelOnChunk bool) ([]agenthost.Registration, []chan agent.TaskResult) {
	regs := make([]agenthost.Registration, 2)
	results := []chan agent.TaskResult{make(chan agent.TaskResult, 64), make(chan agent.TaskResult, 64)}
	for i := range regs {
		regs[i] = agenthost.Registration{Name: "tasks/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handlers[i]}, Factory: func(agent.FactoryArgs) agent.Agent {
			return &taskHarnessAgent{specs: specs, results: results[i], cancelOnChunk: cancelOnChunk}
		}, Bounds: agenthost.Bounds{MaxStateBytes: 4096}}
	}
	return regs, results
}
func takeTaskLaunch(t *testing.T, ch <-chan taskLaunch) taskLaunch {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(4 * time.Second):
		t.Fatal("missing task launch")
		return taskLaunch{}
	}
}
func takeTaskFinal(t *testing.T, ch <-chan agent.TaskResult) []agent.TaskResult {
	t.Helper()
	var all []agent.TaskResult
	for {
		select {
		case r := <-ch:
			all = append(all, r)
			if r.Final {
				return all
			}
		case <-time.After(4 * time.Second):
			t.Fatal("missing terminal task result")
			return nil
		}
	}
}

type taskSnapshot struct {
	State struct {
		Agent struct {
			State struct{ Bytes []byte }
			V2    struct {
				Tasks []struct {
					ID            agent.TaskID `json:"i"`
					Phase         string       `json:"p"`
					Cursor        uint64       `json:"u"`
					Published     uint64       `json:"n"`
					Final         bool         `json:"f"`
					Request       []byte       `json:"q"`
					RequestLength int          `json:"l"`
				}
			}
		}
	}
}

func readTaskSnapshot(t *testing.T, store sessionstore.Store, id string, predicate func(taskSnapshot) bool) taskSnapshot {
	t.Helper()
	var snap taskSnapshot
	require.Eventually(t, func() bool {
		data, err := store.GetState(context.Background(), id)
		return err == nil && json.Unmarshal(data, &snap) == nil && predicate(snap)
	}, 3*time.Second, 2*time.Millisecond)
	return snap
}
func taskCheckpointed(t *testing.T, store sessionstore.Store, id string) taskSnapshot {
	return readTaskSnapshot(t, store, id, func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) > 0 })
}

func TestV2TasksMoveMailbox(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				launches := []chan taskLaunch{make(chan taskLaunch, 8), make(chan taskLaunch, 8)}
				var handlers [2]agent.TaskHandler
				for i := range handlers {
					handlers[i] = func(ctx context.Context, run *agent.TaskRun) error {
						launches[i] <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
						if i == 0 && !pending {
							<-ctx.Done()
							return ctx.Err()
						}
						return run.Emit(ctx, run.After()+1, agent.Chunk{Data: []byte("exact reply")})
					}
				}
				spec := agent.TaskSpec{Step: "first", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Retry, Request: []byte("move-only request")}
				regs, results := taskRegistrations([]agent.TaskSpec{spec}, handlers, false)
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
				if pending {
					require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true))
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				original := takeTaskLaunch(t, launches[0])
				if pending {
					readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final })
				}
				snap := taskCheckpointed(t, store, call.SessionID())
				require.Empty(t, snap.State.Agent.V2.Tasks[0].Request)
				require.Equal(t, len(spec.Request), snap.State.Agent.V2.Tasks[0].RequestLength)
				_, err = sys.plane.Move(ctx, call.SessionID(), "1")
				require.NoError(t, err)
				all := takeTaskFinal(t, results[1])
				require.Len(t, all, 2)
				require.Equal(t, "exact reply", string(all[0].Chunk.Data))
				require.Equal(t, agent.OK, all[1].Outcome)
				require.Equal(t, original.id, all[1].TaskID)
				if pending {
					require.Empty(t, launches[1], "terminal pending results must move without repeating handler")
				} else {
					retry := takeTaskLaunch(t, launches[1])
					require.Equal(t, original.id, retry.id)
					require.Equal(t, spec.Request, retry.request)
				}
				require.Empty(t, results[0])
				committed := readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool {
					var state taskHarnessState
					return json.Unmarshal(s.State.Agent.State.Bytes, &state) == nil && len(state.Results) == 2
				})
				var history taskHarnessState
				require.NoError(t, json.Unmarshal(committed.State.Agent.State.Bytes, &history))
				require.Len(t, history.Results, 2)
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestV2TasksTakeoverRetryAndUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("crash matrix; fast task behavior remains in unit and move tests")
	}
	for _, scenario := range []string{"inflight", "pending", "lost-request"} {
		t.Run(scenario, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				launches := []chan taskLaunch{make(chan taskLaunch, 8), make(chan taskLaunch, 8)}
				var handlers [2]agent.TaskHandler
				for i := range handlers {
					handlers[i] = func(ctx context.Context, run *agent.TaskRun) error {
						launches[i] <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
						if i == 0 && scenario != "pending" {
							<-ctx.Done()
							return ctx.Err()
						}
						return run.Emit(ctx, run.After()+1, agent.Chunk{Data: []byte("regenerated")})
					}
				}
				spec := agent.TaskSpec{Step: "first", Kind: "test/v1", Class: agent.ReadOnly, Resume: agent.Retry}
				if scenario == "lost-request" {
					spec.Request = []byte("lost on crash")
				}
				regs, results := taskRegistrations([]agent.TaskSpec{spec}, handlers, false)
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
				if scenario == "pending" {
					require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true))
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				original := takeTaskLaunch(t, launches[0])
				taskCheckpointed(t, store, call.SessionID())
				if scenario == "pending" {
					readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final })
				}
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				all := takeTaskFinal(t, results[1])
				require.Equal(t, original.id, all[len(all)-1].TaskID)
				if scenario == "lost-request" {
					require.Equal(t, agent.OutcomeUnknown, all[0].Outcome)
					require.Empty(t, launches[1])
				} else {
					require.Len(t, all, 2)
					require.Equal(t, agent.OK, all[1].Outcome)
					retry := takeTaskLaunch(t, launches[1])
					require.Equal(t, original.id, retry.id)
				}
				require.Empty(t, results[0])
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestV2TasksSideEffectRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("extended Retry/Abandon matrix; forced-write gate has a fast test")
	}
	for _, crash := range []bool{false, true} {
		for _, policy := range []agent.ResumePolicy{agent.Retry, agent.Abandon} {
			t.Run(fmt.Sprintf("crash=%v/%s", crash, policy), func(t *testing.T) {
				forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
					launches := []chan taskLaunch{make(chan taskLaunch, 8), make(chan taskLaunch, 8)}
					var ledger sync.Map
					var effects atomic.Int32
					var handlers [2]agent.TaskHandler
					for i := range handlers {
						handlers[i] = func(ctx context.Context, run *agent.TaskRun) error {
							if _, loaded := ledger.LoadOrStore(run.Key(), true); !loaded {
								effects.Add(1)
							}
							launches[i] <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
							if i == 0 {
								<-ctx.Done()
								return ctx.Err()
							}
							return nil
						}
					}
					spec := agent.TaskSpec{Step: "effect", Kind: "test/v1", Class: agent.SideEffecting, Resume: policy, Request: []byte("persisted arguments")}
					regs, results := taskRegistrations([]agent.TaskSpec{spec}, handlers, false)
					sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					call, err := sys.h.Dial(ctx, CallOptions{})
					require.NoError(t, err)
					original := takeTaskLaunch(t, launches[0])
					snap := taskCheckpointed(t, store, call.SessionID())
					require.Equal(t, "may_have_started", snap.State.Agent.V2.Tasks[0].Phase)
					require.Equal(t, spec.Request, snap.State.Agent.V2.Tasks[0].Request)
					if crash {
						require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
					} else {
						_, err = sys.plane.Move(ctx, call.SessionID(), "1")
						require.NoError(t, err)
					}
					all := takeTaskFinal(t, results[1])
					require.Len(t, all, 1)
					if policy == agent.Retry {
						retry := takeTaskLaunch(t, launches[1])
						require.Equal(t, original.key, retry.key)
						require.Equal(t, original.request, retry.request)
						require.Equal(t, agent.OK, all[0].Outcome)
					} else {
						require.Equal(t, agent.OutcomeUnknown, all[0].Outcome)
						require.Empty(t, launches[1])
					}
					require.EqualValues(t, 1, effects.Load())
					_, err = call.Hangup(ctx)
					require.NoError(t, err)
				})
			})
		}
	}
}

func TestV2TasksFifthQueuesAndMoves(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		launches := []chan taskLaunch{make(chan taskLaunch, 8), make(chan taskLaunch, 8)}
		var handlers [2]agent.TaskHandler
		for i := range handlers {
			handlers[i] = func(ctx context.Context, run *agent.TaskRun) error {
				launches[i] <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
				if i == 0 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
		}
		specs := make([]agent.TaskSpec, 5)
		for i := range specs {
			specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly, Request: []byte{byte(i)}}
		}
		specs[4].Class = agent.SideEffecting
		specs[4].Resume = agent.Abandon
		regs, results := taskRegistrations(specs, handlers, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		for range 4 {
			takeTaskLaunch(t, launches[0])
		}
		require.Empty(t, launches[0])
		snap := taskCheckpointed(t, store, call.SessionID())
		require.Len(t, snap.State.Agent.V2.Tasks, 5)
		require.Equal(t, "never_launched", snap.State.Agent.V2.Tasks[4].Phase)
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		seen := map[string]bool{}
		for range 5 {
			launch := takeTaskLaunch(t, launches[1])
			seen[string(launch.request)] = true
			all := takeTaskFinal(t, results[1])
			require.Equal(t, agent.OK, all[0].Outcome)
		}
		require.Len(t, seen, 5)
		require.Empty(t, launches[0])
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2TasksCancellationDropsLate(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		late := make(chan error, 1)
		handler := func(ctx context.Context, run *agent.TaskRun) error {
			if err := run.Emit(ctx, 1, agent.Chunk{Data: []byte("first")}); err != nil {
				return err
			}
			<-ctx.Done()
			late <- run.Emit(context.Background(), 2, agent.Chunk{Data: []byte("late")})
			return nil
		}
		regs, results := taskRegistrations([]agent.TaskSpec{{Step: "cancel", Kind: "test/v1", Class: agent.ReadOnly}}, [2]agent.TaskHandler{handler, handler}, true)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		all := takeTaskFinal(t, results[0])
		require.Len(t, all, 2)
		require.Equal(t, agent.Canceled, all[1].Outcome)
		select {
		case err := <-late:
			require.ErrorIs(t, err, context.Canceled)
		case <-ctx.Done():
			t.Fatal("handler cancellation did not arrive")
		}
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		require.Empty(t, results[1])
		require.Empty(t, results[0])
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type taskOutageStore struct {
	uncertain bool
	sessionstore.Store
	blocked atomic.Bool
	failed  atomic.Int32
	source  netip.AddrPort
}

func (s *taskOutageStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	var snap taskSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if lease.Worker == s.source && len(snap.State.Agent.V2.Tasks) > 0 && s.blocked.Load() {
		s.failed.Add(1)
		if s.uncertain {
			if err := s.Store.PutState(ctx, lease, data); err != nil {
				return err
			}
		}
		return sessionstore.ErrTransient
	}
	return s.Store.PutState(ctx, lease, data)
}
func TestV2TasksForcedCheckpointOutage(t *testing.T) {
	for _, mode := range []string{"recover", "expire", "uncertain"} {
		expired := mode == "expire"
		t.Run(mode, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				gate := &taskOutageStore{Store: store, uncertain: mode == "uncertain"}
				gate.blocked.Store(true)
				launched := make(chan taskLaunch, 8)
				handler := func(_ context.Context, run *agent.TaskRun) error {
					launched <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
					return nil
				}
				timeout := 5 * time.Second
				if expired {
					timeout = 3 * time.Second
				}
				specs := []agent.TaskSpec{{Step: "effect", Kind: "test/v1", Class: agent.SideEffecting, Resume: agent.Abandon, Timeout: timeout}}
				regs, results := taskRegistrations(specs, [2]agent.TaskHandler{handler, handler}, false)
				// Suppress coalesced writes while the intent differs from the last
				// acknowledged state, so every observed failure is a forced retry.
				sys := startAgentHarness(t, gate, agentHarnessSettings{v2: regs, saveInterval: time.Hour})
				gate.source = sys.workers[0].LocalAddr()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				deadline := time.NewTimer(4 * time.Second)
				tick := time.NewTicker(time.Millisecond)
				for gate.failed.Load() < 5 {
					select {
					case launch := <-launched:
						t.Fatalf("task launched before durable acknowledgment: %+v", launch)
					case <-tick.C:
					case <-deadline.C:
						t.Fatal("fewer than five failed forced writes")
					}
				}
				tick.Stop()
				deadline.Stop()
				require.Empty(t, launched)
				require.Empty(t, results[0], "outage must never produce Capacity")
				if expired {
					all := takeTaskFinal(t, results[0])
					require.Equal(t, agent.Timeout, all[0].Outcome)
					require.Empty(t, launched)
				}
				gate.blocked.Store(false)
				if expired {
					_, err = sys.plane.Move(ctx, call.SessionID(), "1")
					require.NoError(t, err)
					readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool {
						var history taskHarnessState
						return json.Unmarshal(s.State.Agent.State.Bytes, &history) == nil && len(history.Results) == 1 && history.Results[0].Outcome == agent.Timeout
					})
					require.Empty(t, launched, "expired task cannot launch after store recovery")
				}
				if !expired {
					takeTaskLaunch(t, launched)
					all := takeTaskFinal(t, results[0])
					require.Equal(t, agent.OK, all[0].Outcome)
				}
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

func TestV2TasksRetryPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("extended prefix replay matrix")
	}
	for _, crash := range []bool{false, true} {
		for _, class := range []agent.TaskClass{agent.ReadOnly, agent.SideEffecting} {
			t.Run(fmt.Sprintf("crash=%v/%s", crash, class), func(t *testing.T) {
				forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
					launches := []chan taskLaunch{make(chan taskLaunch, 8), make(chan taskLaunch, 8)}
					var handlers [2]agent.TaskHandler
					for i := range handlers {
						handlers[i] = func(ctx context.Context, run *agent.TaskRun) error {
							launches[i] <- taskLaunch{run.ID(), run.Key(), run.After(), run.Spec().Request}
							if err := run.Emit(ctx, run.After()+1, agent.Chunk{Data: []byte(fmt.Sprint(run.After() + 1))}); err != nil {
								return err
							}
							if i == 0 {
								<-ctx.Done()
								return ctx.Err()
							}
							return nil
						}
					}
					specs := []agent.TaskSpec{{Step: "prefix", Kind: "test/v1", Class: class, Resume: agent.Retry}}
					regs, results := taskRegistrations(specs, handlers, false)
					sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					call, err := sys.h.Dial(ctx, CallOptions{})
					require.NoError(t, err)
					first := takeTaskLaunch(t, launches[0])
					readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool {
						return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Cursor == 1
					})
					if crash {
						require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
					} else {
						_, err = sys.plane.Move(ctx, call.SessionID(), "1")
						require.NoError(t, err)
					}
					retry := takeTaskLaunch(t, launches[1])
					require.Equal(t, first.key, retry.key)
					require.EqualValues(t, 1, retry.after)
					all := takeTaskFinal(t, results[1])
					require.Len(t, all, 2)
					require.EqualValues(t, 2, all[0].Sequence)
					require.Equal(t, "2", string(all[0].Chunk.Data))
					require.EqualValues(t, 3, all[1].Sequence)
					committed := readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool {
						var state taskHarnessState
						return json.Unmarshal(s.State.Agent.State.Bytes, &state) == nil && len(state.Results) == 3
					})
					var history taskHarnessState
					require.NoError(t, json.Unmarshal(committed.State.Agent.State.Bytes, &history))
					require.Len(t, history.Results, 3)
					_, err = call.Hangup(ctx)
					require.NoError(t, err)
				})
			})
		}
	}
}
