package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueuedRelayChangeCannotStarveTakeover(t *testing.T) {
	c := &call{}
	require.NoError(t, c.lockControl(context.Background()))
	relayCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		if c.lockOperatorCancel(relayCtx, false, cancel) == nil {
			<-relayCtx.Done()
			c.controlMu.Lock()
			c.relayCancel = nil
			c.mu.Unlock()
			c.controlMu.Unlock()
		}
	}()
	waiters := func() int { c.controlMu.Lock(); defer c.controlMu.Unlock(); return c.controlWaiters }
	require.Eventually(t, func() bool { return waiters() == 1 }, time.Second, time.Millisecond)
	taken := make(chan error, 1)
	go func() {
		err := c.lockOperator(context.Background(), true)
		if err == nil {
			c.mu.Unlock()
		}
		taken <- err
	}()
	require.Eventually(t, func() bool {
		select {
		case <-relayDone: // yielding before Control releases is also valid
			return true
		default:
			return waiters() == 2
		}
	}, time.Second, time.Millisecond)
	c.unlockControl()
	err := <-taken
	cancel()
	<-relayDone
	require.NoError(t, err, "a previously queued relay operation must yield or be cancelled before the takeover deadline")
}
