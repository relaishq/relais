package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestartWorkerAvoidsRegisteredAddress(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	addresses := []string{h.workers.list[0].LocalAddr().String(), h.workers.list[1].LocalAddr().String()}
	for worker := range h.workers.list {
		require.NoError(t, h.Kill(worker))
	}
	require.Eventually(t, func() bool {
		status, err := h.Status(context.Background())
		if err != nil || len(status.Workers) != 2 {
			return false
		}
		for _, worker := range status.Workers {
			if !worker.Dead || worker.Recovering {
				return false
			}
		}
		return true
	}, 2*time.Second, 5*time.Millisecond)

	allocations := 0
	h.workers.relay.newWorker = func(cfg mediaworker.Config) (*mediaworker.Worker, error) {
		allocations++
		if allocations <= len(addresses) {
			// Force another dead registration's address, then our own.
			cfg.ListenAddr = addresses[len(addresses)-allocations]
		}
		return mediaworker.New(cfg)
	}
	require.NoError(t, h.RestartWorker(0))
	require.Equal(t, 3, allocations)
	require.NotContains(t, addresses, h.workers.list[0].LocalAddr().String())
	status, err := h.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Workers, 2)
	assert.False(t, status.Workers[0].Dead)
	assert.True(t, status.Workers[1].Dead)
	assert.Equal(t, addresses[1], status.Workers[1].Address.String())
}
