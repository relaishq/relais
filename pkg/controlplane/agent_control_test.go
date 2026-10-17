package controlplane

import (
	"context"
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
