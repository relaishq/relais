package callharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
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
		go func() {
			for {
				_, err := sys.plane.Move(ctx, id, "1")
				if !errors.Is(err, controlplane.ErrMoveInProgress) {
					moved <- err
					return
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					moved <- ctx.Err()
					return
				}
			}
		}()
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
