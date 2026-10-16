package callharness

import (
	"context"
	"errors"
	"fmt"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerprobe"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestV2TaskReviewCapacityDoesNotEndCall(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
		specs := make([]agent.TaskSpec, 12)
		for i := range specs {
			specs[i] = agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("r")}
		}
		regs, results := taskRegistrations(specs, [2]agent.TaskHandler{handler, handler}, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		select {
		case result := <-results[0]:
			require.Equal(t, agent.Capacity, result.Outcome)
			require.True(t, result.Final)
		case <-time.After(2 * time.Second):
			t.Fatal("refused task did not produce Capacity")
		}
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		require.Equal(t, 1, sys.workers[0].SessionCount())
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func startTaskReviewHarness3(t *testing.T, store sessionstore.Store, regs []agenthost.Registration) *agentHarnessSystem {
	t.Helper()
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	sys := &agentHarnessSystem{plane: plane, store: store}
	for i := range 3 {
		w, err := mediaworker.New(mediaworker.Config{Agent: mediaworker.AgentConfig{Factory: func(id string) legacy.Agent {
			return agenthost.Factory(regs[i])(id)
		}}, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		sys.workers = append(sys.workers, w)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		r.AddWorker(w.LocalAddr())
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), w))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	server := httptest.NewServer(plane.Handler())
	t.Cleanup(server.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	sys.h = h
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return sys
}

type taskReviewInputAgent struct {
	taskHarnessAgent
	fired   bool
	resumed chan agent.Outcome
}

func (a *taskReviewInputAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Resumed && a.resumed != nil {
		a.resumed <- e.Resume.MoveOnly
	}
	if e.Kind == agent.Started {
		return agent.Effects{}, nil
	}
	if e.Kind == agent.AudioInput && !a.fired {
		a.fired = true
		return agent.Effects{StartTasks: a.specs}, nil
	}
	return a.taskHarnessAgent.Handle(ctx, e)
}
func (a *taskReviewInputAgent) Save(ctx context.Context) (agent.State, error) {
	s, err := a.taskHarnessAgent.Save(ctx)
	if a.fired {
		s.Version = 2
	}
	return s, err
}
func (a *taskReviewInputAgent) Restore(ctx context.Context, s agent.State) error {
	a.fired = s.Version == 2
	return a.taskHarnessAgent.Restore(ctx, s)
}
func TestV2TaskReviewMoveAfterTakeover(t *testing.T) {
	if testing.Short() {
		t.Skip("three-worker crash and move matrix")
	}
	for _, continuous := range []bool{false, true} {
		t.Run(fmt.Sprint(continuous), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				runTaskReviewMoveAfterTakeover(t, store, continuous)
			})
		})
	}
}
func TestV2TaskReviewEmptyRequestTakeover(t *testing.T) {
	if testing.Short() {
		t.Skip("crash recovery on both task classes")
	}
	for _, class := range []agent.TaskClass{agent.ReadOnly, agent.SideEffecting} {
		t.Run(string(class), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				launches := []chan taskLaunch{make(chan taskLaunch, 4), make(chan taskLaunch, 4)}
				var handlers [2]agent.TaskHandler
				for i := range handlers {
					handlers[i] = func(ctx context.Context, r *agent.TaskRun) error {
						launches[i] <- taskLaunch{id: r.ID(), key: r.Key()}
						if i == 0 {
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					}
				}
				regs, results := taskRegistrations([]agent.TaskSpec{{Step: "empty", Kind: "test/v1", Class: class, Resume: agent.Retry, Request: []byte{}}}, handlers, false)
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				first := takeTaskLaunch(t, launches[0])
				taskCheckpointed(t, store, call.SessionID())
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				next := takeTaskLaunch(t, launches[1])
				require.Equal(t, first.key, next.key)
				all := takeTaskFinal(t, results[1])
				require.Equal(t, agent.OK, all[len(all)-1].Outcome)
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

// Keep a long CJK diagnostic pending so the private export, JSON decode and
// target mailbox validator all see its bounded UTF-8 representation.
func TestV2TaskReviewDetailMove(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		handler := func(_ context.Context, r *agent.TaskRun) error {
			return r.Fail(agent.Failed, "provider error:: "+strings.Repeat("超时", 60))
		}
		regs, results := taskRegistrations([]agent.TaskSpec{{Step: "detail", Kind: "test/v1", Class: agent.ReadOnly}}, [2]agent.TaskHandler{handler, handler}, false)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true})
		require.NoError(t, workerprobe.SetHoldTaskResults(sys.workers[0].LocalAddr(), true))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool { return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final })
		moved, err := sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		require.False(t, moved.Lost)
		all := takeTaskFinal(t, results[1])
		require.Equal(t, agent.Failed, all[0].Outcome)
		require.LessOrEqual(t, len(all[0].Detail), 256)
		require.True(t, utf8.ValidString(all[0].Detail))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type taskReviewFailingResultAgent struct {
	taskHarnessAgent
	timeout bool
	entered chan struct{}
	input   bool
	fired   bool
}

func (a *taskReviewFailingResultAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if a.input {
		if e.Kind == agent.Started {
			return agent.Effects{}, nil
		}
		if e.Kind == agent.AudioInput && !a.fired {
			a.fired = true
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: strings.Repeat("x", 4096), Kind: "test/v1", Class: agent.ReadOnly}}}, nil
		}
	}
	if e.Kind == agent.TaskMessage {
		close(a.entered)
		if a.timeout {
			<-ctx.Done()
			return agent.Effects{}, ctx.Err()
		}
		return agent.Effects{}, errors.New("task result callback failed")
	}
	return a.taskHarnessAgent.Handle(ctx, e)
}
func TestV2TaskReviewTaskMessageFailsClosed(t *testing.T) {
	for _, tc := range []struct{ timeout, input bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("deadline=%v/capacity-from-input=%v", tc.timeout, tc.input), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				entered := make(chan struct{})
				handler := func(context.Context, *agent.TaskRun) error { return nil }
				reg := agenthost.Registration{Name: "fail-closed/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handler}, Factory: func(agent.FactoryArgs) agent.Agent {
					return &taskReviewFailingResultAgent{taskHarnessAgent: taskHarnessAgent{specs: []agent.TaskSpec{{Step: "result", Kind: "test/v1", Class: agent.ReadOnly}}}, timeout: tc.timeout, entered: entered, input: tc.input}
				}}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				if tc.input {
					_ = call.SendMedia(ctx, 100*time.Millisecond)
				}
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("result callback never ran")
				}
				require.Eventually(t, func() bool { return sys.workers[0].SessionCount() == 0 }, 2*time.Second, time.Millisecond)
				if tc.timeout {
					require.Positive(t, sys.workers[0].AgentStats().CallbackDeadlines)
				} else {
					require.Positive(t, sys.workers[0].AgentStats().CallbackFailures)
				}
			})
		})
	}
}

func TestV2TaskReviewMoveAfterUncertainRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("uncertain resume rollback and subsequent move")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		launched := make(chan struct{}, 4)
		results := make(chan agent.TaskResult, 16)
		handler := func(ctx context.Context, _ *agent.TaskRun) error {
			launched <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		reg := agenthost.Registration{Name: "rollback/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handler}, Bounds: agenthost.Bounds{MaxStateBytes: 4096}, Factory: func(agent.FactoryArgs) agent.Agent {
			return &taskReviewInputAgent{taskHarnessAgent: taskHarnessAgent{specs: []agent.TaskSpec{{Step: "r", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("recording")}}, results: results}}
		}}
		var refused atomic.Bool
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, remoteTarget: true, targetHandler: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resume" && refused.CompareAndSwap(false, true) {
					privateapi.Error(w, context.DeadlineExceeded, nil)
					return
				}
				next.ServeHTTP(w, r)
			})
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 80*time.Millisecond))
		select {
		case <-launched:
		case <-ctx.Done():
			t.Fatal("missing source task")
		}
		taskCheckpointed(t, store, call.SessionID())
		first, err := sys.plane.Move(ctx, call.SessionID(), "1")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.True(t, first.Result.RolledBack)
		require.False(t, first.Lost)
		all := takeTaskFinal(t, results)
		require.Equal(t, agent.OutcomeUnknown, all[len(all)-1].Outcome)
		next, err := sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		require.False(t, next.Lost)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestV2TaskReviewCompletedRequestIsNotLostData(t *testing.T) {
	if testing.Short() {
		t.Skip("completed task crash notice")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		results := make(chan agent.TaskResult, 8)
		resumed := make(chan agent.Outcome, 4)
		handler := func(context.Context, *agent.TaskRun) error { return nil }
		reg := agenthost.Registration{Name: "completed/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handler}, Factory: func(agent.FactoryArgs) agent.Agent {
			return &taskReviewInputAgent{taskHarnessAgent: taskHarnessAgent{specs: []agent.TaskSpec{{Step: "done", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("recording")}}, results: results}, resumed: resumed}
		}}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 80*time.Millisecond))
		takeTaskFinal(t, results)
		readTaskSnapshot(t, store, call.SessionID(), func(s taskSnapshot) bool {
			return len(s.State.Agent.V2.Tasks) == 1 && s.State.Agent.V2.Tasks[0].Final && s.State.Agent.V2.Tasks[0].Cursor == s.State.Agent.V2.Tasks[0].Published
		})
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		select {
		case outcome := <-resumed:
			require.Equal(t, agent.OK, outcome)
		case <-ctx.Done():
			t.Fatal("no resume notice")
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func runTaskReviewMoveAfterTakeover(t *testing.T, store sessionstore.Store, continuous bool) {
	launched := make(chan struct{}, 8)
	results := make(chan agent.TaskResult, 64)
	handler := func(ctx context.Context, _ *agent.TaskRun) error {
		launched <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	spec := agent.TaskSpec{Step: "stt", Kind: "test/v1", Class: agent.ReadOnly, Request: []byte("recording")}
	regs := make([]agenthost.Registration, 3)
	for i := range regs {
		regs[i] = agenthost.Registration{Name: "review/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handler}, Factory: func(agent.FactoryArgs) agent.Agent {
			return &taskReviewInputAgent{taskHarnessAgent: taskHarnessAgent{specs: []agent.TaskSpec{spec}, results: results}}
		}, Bounds: agenthost.Bounds{MaxStateBytes: 4096}}
	}
	sys := startTaskReviewHarness3(t, store, regs)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	call, err := sys.h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	mediaCtx, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	if continuous {
		done := make(chan struct{})
		go func() { defer close(done); _ = call.SendMedia(mediaCtx, 10*time.Second) }()
		defer func() { stopMedia(); <-done }()
	} else {
		require.NoError(t, call.SendMedia(ctx, 80*time.Millisecond))
	}
	select {
	case <-launched:
	case <-ctx.Done():
		t.Fatal("task never launched")
	}
	taskCheckpointed(t, store, call.SessionID())
	lease, err := store.Get(ctx, call.SessionID())
	require.NoError(t, err)
	owner := -1
	for i, w := range sys.workers {
		if w.LocalAddr() == lease.Worker {
			owner = i
		}
	}
	require.GreaterOrEqual(t, owner, 0)
	require.NoError(t, workerprobe.Kill(sys.workers[owner].LocalAddr()))
	final := takeTaskFinal(t, results)
	require.Equal(t, agent.OutcomeUnknown, final[len(final)-1].Outcome)
	// Silent input leaves the input-caused receipt below its duplicate horizon.
	if !continuous {
		time.Sleep(120 * time.Millisecond)
	}
	lease, err = store.Get(ctx, call.SessionID())
	require.NoError(t, err)
	target := -1
	for i, w := range sys.workers {
		if i != owner && w.LocalAddr() != lease.Worker {
			target = i
		}
	}
	require.GreaterOrEqual(t, target, 0)
	moved, err := sys.plane.Move(ctx, call.SessionID(), strconv.Itoa(target))
	require.NoError(t, err)
	require.False(t, moved.Lost)
	require.Equal(t, 1, sys.workers[target].SessionCount())
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}

func TestV2TaskFinalReviewMoveAfterTakeoverShort(t *testing.T) {
	runTaskReviewMoveAfterTakeover(t, sessionstore.NewMemory(), false)
}
