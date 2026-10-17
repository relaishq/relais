package callharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type controlHarnessAgent struct {
	controls []agent.ControlMessage
	events   chan agent.Event
	entered  chan struct{}
	release  chan struct{}
}

func (a *controlHarnessAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Control {
		if a.entered != nil {
			close(a.entered)
			select {
			case <-a.release:
			case <-ctx.Done():
				return agent.Effects{}, ctx.Err()
			}
			a.entered = nil
		}
		saved := *e.Control
		saved.Data = nil
		a.controls = append(a.controls, saved)
	}
	if e.Kind != agent.AudioInput {
		a.events <- e
	}
	return agent.Effects{}, nil
}
func (a *controlHarnessAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a.controls)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *controlHarnessAgent) Restore(_ context.Context, s agent.State) error {
	return json.Unmarshal(s.Bytes, &a.controls)
}

type controlFreezeStore struct {
	sessionstore.Store
	block atomic.Bool
}

func (s *controlFreezeStore) PutState(ctx context.Context, l sessionstore.Lease, b []byte) error {
	if s.block.Load() {
		return sessionstore.ErrTransient
	}
	return s.Store.PutState(ctx, l, b)
}
func controlRegistrations(entered, release chan struct{}) ([]agenthost.Registration, []chan agent.Event) {
	regs := make([]agenthost.Registration, 2)
	events := []chan agent.Event{make(chan agent.Event, 64), make(chan agent.Event, 64)}
	for i := range regs {
		regs[i] = agenthost.Registration{Name: "control-test/v1", Factory: func(agent.FactoryArgs) agent.Agent {
			a := &controlHarnessAgent{events: events[i]}
			if i == 0 {
				a.entered = entered
				a.release = release
			}
			return a
		}}
	}
	return regs, events
}
func awaitControlKind(t *testing.T, ch <-chan agent.Event, kind agent.EventKind) agent.Event {
	t.Helper()
	select {
	case e := <-ch:
		require.Equal(t, kind, e.Kind)
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("missing event", kind)
		return agent.Event{}
	}
}
func waitControlCheckpoint(t *testing.T, sys *agentHarnessSystem, id string, m agent.ControlMessage) agent.ControlReceipt {
	t.Helper()
	var got agent.ControlReceipt
	require.Eventually(t, func() bool {
		var err error
		got, err = sys.plane.AgentControl(context.Background(), id, m)
		return err == nil && got.Checkpointed
	}, 3*time.Second, 5*time.Millisecond)
	return got
}
func TestRoutedControlReceiptsOrderAndMove(t *testing.T) {
	forAgentStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &controlFreezeStore{Store: base}
		regs, events := controlRegistrations(nil, nil)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, remoteTarget: true, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		id := call.SessionID()
		awaitControlKind(t, events[0], agent.Started)
		readV2Snapshot(t, base, id, true)
		store.block.Store(true)
		m := agent.ControlMessage{ID: "press", Sequence: 1, Name: "ptt.start", Data: make([]byte, 1024)}
		post := func(want int) agent.ControlReceipt {
			b, err := json.Marshal(m)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, sys.h.SignalingURL()+"/"+id+"/agent/control", bytes.NewReader(b))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, want, response.StatusCode)
			var receipt agent.ControlReceipt
			require.NoError(t, json.NewDecoder(response.Body).Decode(&receipt))
			return receipt
		}
		receipt := post(http.StatusAccepted)
		require.False(t, receipt.Checkpointed)
		e := awaitControlKind(t, events[0], agent.Control)
		require.Equal(t, receipt.EventID, e.ID)
		require.Equal(t, m, *e.Control)
		again := post(http.StatusAccepted)
		require.Equal(t, receipt, again)
		conflict := m
		conflict.Data = []byte("changed")
		_, err = sys.plane.AgentControl(ctx, id, conflict)
		require.ErrorIs(t, err, agent.ErrControlConflict)
		future := agent.ControlMessage{ID: "future", Sequence: 3, Name: "stop"}
		_, err = sys.plane.AgentControl(ctx, id, future)
		require.ErrorIs(t, err, agent.ErrControlOrder)
		oversize := agent.ControlMessage{ID: "oversize", Sequence: 2, Name: "stop", Data: make([]byte, 1025)}
		_, err = sys.plane.AgentControl(ctx, id, oversize)
		require.ErrorIs(t, err, agent.ErrControlInvalid)
		store.block.Store(false)
		checkpoint := waitControlCheckpoint(t, sys, id, m)
		require.Equal(t, receipt.EventID, checkpoint.EventID)
		require.Equal(t, checkpoint, post(http.StatusOK))
		_, err = sys.plane.Move(ctx, id, "1")
		require.NoError(t, err)
		awaitControlKind(t, events[1], agent.Resumed)
		moved := waitControlCheckpoint(t, sys, id, m)
		require.Equal(t, checkpoint, moved)
		require.Equal(t, moved, post(http.StatusOK))
		for i := uint64(2); i <= 12; i++ {
			m = agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "ptt.stop"}
			_, err = sys.plane.AgentControl(ctx, id, m)
			require.NoError(t, err)
			e = awaitControlKind(t, events[1], agent.Control)
			require.Equal(t, i, e.Control.Sequence)
			waitControlCheckpoint(t, sys, id, m)
		}
		late := agent.ControlMessage{ID: "press", Sequence: 1, Name: "ptt.start", Data: make([]byte, 1024)}
		_, err = sys.plane.AgentControl(ctx, id, late)
		require.ErrorIs(t, err, agent.ErrControlStale)
		// A takeover restores both the compacted floor and the retained receipt.
		require.NoError(t, workerprobe.Kill(sys.workers[1].LocalAddr()))
		awaitControlKind(t, events[0], agent.Resumed)
		_, err = sys.plane.AgentControl(ctx, id, late)
		require.ErrorIs(t, err, agent.ErrControlStale)
		require.Equal(t, m.Sequence, waitControlCheckpoint(t, sys, id, m).Sequence)
		require.Empty(t, events[0])
		require.Empty(t, events[1])
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestRoutedControlCrashBeforeCheckpoint(t *testing.T) {
	forAgentStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &controlFreezeStore{Store: base}
		regs, events := controlRegistrations(nil, nil)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		id := call.SessionID()
		awaitControlKind(t, events[0], agent.Started)
		readV2Snapshot(t, base, id, true)
		store.block.Store(true)
		m := agent.ControlMessage{ID: "crash", Sequence: 1, Name: "ptt.start"}
		receipt, err := sys.plane.AgentControl(ctx, id, m)
		require.NoError(t, err)
		require.False(t, receipt.Checkpointed)
		original := awaitControlKind(t, events[0], agent.Control)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		store.block.Store(false)
		awaitControlKind(t, events[1], agent.Resumed)
		_, err = sys.plane.AgentControl(ctx, id, m)
		require.NoError(t, err)
		retried := awaitControlKind(t, events[1], agent.Control)
		require.Equal(t, original.ID, retried.ID)
		waitControlCheckpoint(t, sys, id, m)
		require.Empty(t, events[1])
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
func TestRoutedControlInFlightMove(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		entered, release := make(chan struct{}), make(chan struct{})
		regs, events := controlRegistrations(entered, release)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: 2 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		id := call.SessionID()
		awaitControlKind(t, events[0], agent.Started)
		m := agent.ControlMessage{ID: "raced", Sequence: 1, Name: "ptt.start"}
		sent := make(chan error, 1)
		go func() { _, err := sys.plane.AgentControl(ctx, id, m); sent <- err }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		moved := make(chan error, 1)
		go func() { _, err := sys.plane.Move(ctx, id, "1"); moved <- err }()
		close(release)
		require.NoError(t, <-sent)
		require.NoError(t, <-moved)
		awaitControlKind(t, events[0], agent.Control)
		awaitControlKind(t, events[1], agent.Resumed)
		waitControlCheckpoint(t, sys, id, m)
		require.Empty(t, events[1])
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

type refusingControlHarnessAgent struct {
	controlHarnessAgent
	failure *atomic.Bool
	slow    bool
}

func (a *refusingControlHarnessAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Control && a.failure.Load() {
		if a.slow {
			<-ctx.Done()
			return agent.Effects{}, ctx.Err()
		}
		return agent.Effects{}, errors.New("provider unavailable")
	}
	return a.controlHarnessAgent.Handle(ctx, e)
}
func TestRoutedControlHandlerRefusalKeepsCallAlive(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprint(slow), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				events := make(chan agent.Event, 32)
				var failure atomic.Bool
				failure.Store(true)
				reg := agenthost.Registration{Name: "control-refusal/v1", Factory: func(agent.FactoryArgs) agent.Agent {
					return &refusingControlHarnessAgent{controlHarnessAgent: controlHarnessAgent{events: events}, failure: &failure, slow: slow}
				}}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, callbackBudget: 250 * time.Millisecond})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				id := call.SessionID()
				awaitControlKind(t, events, agent.Started)
				m := agent.ControlMessage{ID: "retry", Sequence: 1, Name: "start"}
				_, err = sys.plane.AgentControl(ctx, id, m)
				require.Error(t, err)
				// The same sequence succeeds after the provider/handler recovers.
				failure.Store(false)
				receipt, err := sys.plane.AgentControl(ctx, id, m)
				require.NoError(t, err)
				require.Equal(t, uint64(1), receipt.Sequence)
				awaitControlKind(t, events, agent.Control)
				waitControlCheckpoint(t, sys, id, m)
				_, err = sys.plane.Move(ctx, id, "1")
				require.NoError(t, err)
				awaitControlKind(t, events, agent.Resumed)
				receipt, err = sys.plane.AgentControl(ctx, id, m)
				require.NoError(t, err)
				require.True(t, receipt.Checkpointed)
				require.Empty(t, events)
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
func TestRoutedControlRetriesDoNotStarveOperators(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(fmt.Sprint(drain), func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				entered, release := make(chan struct{}), make(chan struct{})
				regs, events := controlRegistrations(entered, release)
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, callbackBudget: 2 * time.Second})
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				id := call.SessionID()
				awaitControlKind(t, events[0], agent.Started)
				m := agent.ControlMessage{ID: "press", Sequence: 1, Name: "start"}
				sent := make(chan error, 1)
				go func() { _, err := sys.plane.AgentControl(ctx, id, m); sent <- err }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				// Keep identical retries active across the operator request and route change.
				retryCtx, stop := context.WithCancel(ctx)
				retried := make(chan struct{})
				go func() {
					defer close(retried)
					for retryCtx.Err() == nil {
						_, _ = sys.plane.AgentControl(retryCtx, id, m)
						select {
						case <-retryCtx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
				moved := make(chan error, 1)
				go func() {
					if drain {
						_, err := sys.plane.Drain(ctx, "0")
						moved <- err
					} else {
						_, err := sys.plane.Move(ctx, id, "1")
						moved <- err
					}
				}()
				// Hold the first callback long enough to overlap the operator request.
				time.Sleep(30 * time.Millisecond)
				close(release)
				require.NoError(t, <-sent)
				require.NoError(t, <-moved)
				stop()
				<-retried
				awaitControlKind(t, events[0], agent.Control)
				awaitControlKind(t, events[1], agent.Resumed)
				waitControlCheckpoint(t, sys, id, m)
				require.Empty(t, events[1])
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

type budgetControlHarnessAgent struct {
	controlHarnessAgent
	trigger *atomic.Bool
}

func (a *budgetControlHarnessAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	effects, err := a.controlHarnessAgent.Handle(ctx, e)
	if e.Kind == agent.AudioInput && a.trigger.Swap(false) {
		for i := range 4 {
			effects.SetTimers = append(effects.SetTimers, agent.TimerSpec{ID: fmt.Sprintf("%d", i) + strings.Repeat("t", 63), After: time.Hour})
		}
		effects.StartTasks = []agent.TaskSpec{{Step: "wait", Kind: "wait/v1", Class: agent.ReadOnly}}
	}
	return effects, err
}
func TestRoutedControlFullBudgetAllowsInputEffects(t *testing.T) {
	forAgentStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &controlFreezeStore{Store: base}
		events := make(chan agent.Event, 64)
		ran := make(chan struct{}, 1)
		var trigger atomic.Bool
		reg := agenthost.Registration{Name: "control-budget/v1", Factory: func(agent.FactoryArgs) agent.Agent {
			return &budgetControlHarnessAgent{controlHarnessAgent: controlHarnessAgent{events: events}, trigger: &trigger}
		}, Tasks: map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { ran <- struct{}{}; return nil }}}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		id := call.SessionID()
		awaitControlKind(t, events, agent.Started)
		readV2Snapshot(t, base, id, true)
		store.block.Store(true)
		accepted := uint64(0)
		for i := uint64(1); i <= 40; i++ {
			m := agent.ControlMessage{ID: fmt.Sprintf("%03d", i) + strings.Repeat("x", 125), Sequence: i, Name: "press"}
			receipt, err := sys.plane.AgentControl(ctx, id, m)
			if err != nil {
				require.Error(t, err)
				break
			}
			require.False(t, receipt.Checkpointed)
			accepted++
			awaitControlKind(t, events, agent.Control)
		}
		require.Positive(t, accepted)
		require.Less(t, accepted, uint64(40))
		t.Logf("accepted %d unacknowledged Controls", accepted)
		trigger.Store(true)
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatal("full Control budget prevented ordinary input task/timer effects")
		}
		awaitControlKind(t, events, agent.TaskMessage)
		store.block.Store(false)
		_, err = sys.plane.Move(ctx, id, "1")
		require.NoError(t, err)
		awaitControlKind(t, events, agent.Resumed)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
