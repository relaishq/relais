package controlplane

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestControlRouteWaitHasDefaultBudget(t *testing.T) {
	p, _, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	c, err := p.lookup(id)
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := p.AgentControl(context.Background(), id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("route lock has no bounded outcome")
	}
}

type blockingControlWorker struct {
	*fakeWorker
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingControlWorker) AgentControl(ctx context.Context, _ string, m agent.ControlMessage) (agent.ControlReceipt, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
		return agent.ControlReceipt{ID: m.ID, Sequence: m.Sequence, EventID: "control/x"}, nil
	case <-ctx.Done():
		return agent.ControlReceipt{}, ctx.Err()
	}
}
func TestControlInFlightAllowsMoveAndDrain(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(map[bool]string{false: "move", true: "drain"}[drain], func(t *testing.T) {
			p, base, _, _ := setup(t)
			w := &blockingControlWorker{fakeWorker: base, entered: make(chan struct{}), release: make(chan struct{})}
			p.workers["a"].worker = w
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			sent := make(chan error, 1)
			go func() {
				_, err := p.AgentControl(ctx, id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
				sent <- err
			}()
			<-w.entered
			moved := make(chan error, 1)
			go func() {
				if drain {
					_, err := p.Drain(ctx, "a")
					moved <- err
				} else {
					_, err := p.Move(ctx, id, "b")
					moved <- err
				}
			}()
			select {
			case err := <-moved:
				close(w.release)
				<-sent
				require.NoError(t, err, "operator must wait for Control instead of failing")
			case <-time.After(30 * time.Millisecond):
				close(w.release)
				require.NoError(t, <-sent)
				require.NoError(t, <-moved)
			}
		})
	}
}
func TestControlInvalidDoesNotWaitForRouteLock(t *testing.T) {
	p, _, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	c, err := p.lookup(id)
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.AgentControl(ctx, id, agent.ControlMessage{ID: strings.Repeat("x", 129), Sequence: 1, Name: "start"})
	require.ErrorIs(t, err, agent.ErrControlInvalid)
}
func TestControlRouteHoldFitsTakeoverBudget(t *testing.T) {
	p, base, _, _ := setup(t)
	w := &blockingControlWorker{fakeWorker: base, entered: make(chan struct{}), release: make(chan struct{})}
	p.workers["a"].worker = w
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	started := time.Now()
	_, err = p.AgentControl(context.Background(), id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
	require.Error(t, err)
	require.Less(t, time.Since(started), takeoverBudget, "Control route holder must leave takeover time")
}

func TestControlTakeoverCancelsInFlightRoute(t *testing.T) {
	p, base, target, _ := setup(t)
	w := &blockingControlWorker{fakeWorker: base, entered: make(chan struct{}), release: make(chan struct{})}
	p.workers["a"].worker = w
	p.workers["b"].worker = &takeoverWorker{fakeWorker: target}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	sent := make(chan error, 1)
	go func() {
		_, err := p.AgentControl(ctx, id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
		sent <- err
	}()
	<-w.entered
	p.mu.Lock()
	source := p.workers["a"]
	source.dead = true
	p.mu.Unlock()
	start := time.Now()
	p.takeover(ctx, source, lease, start)
	require.Less(t, time.Since(start), takeoverBudget)
	require.Error(t, <-sent)
	current, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, target.addr, current.Worker)
	require.True(t, target.runs(id))
}
