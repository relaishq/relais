//go:build !windows

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

func TestRelayLossBaselineWaitsForQueuedWorkerPackets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		polls := 0
		baseline, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			polls++
			s := relay.RelayStatus{Stats: relay.Stats{WorkerPackets: 10, CallerPackets: uint64(polls)}}
			// HTTP answers twice with the old count before queued UDP drains.
			if time.Since(started) >= 50*time.Millisecond {
				s.Stats.WorkerPackets = 20
			}
			if time.Since(started) >= 225*time.Millisecond {
				s.Stats.WorkerPackets = 30
			}
			return s, nil
		})
		require.NoError(t, err)
		require.Equal(t, uint64(30), baseline.Stats.WorkerPackets)
		require.Equal(t, 425*time.Millisecond, time.Since(started), "each late drain resets the quiet window; caller traffic need not settle")
	})
}

func TestRelayLossBaselineAcceptsIdleWorkerCounter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		_, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			return relay.RelayStatus{}, nil
		})
		require.NoError(t, err)
		require.Equal(t, 200*time.Millisecond, time.Since(started))
	})
}

func TestRelayLossBaselineRejectsCounterThatNeverSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var packets uint64
		last, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
			packets++
			return relay.RelayStatus{Stats: relay.Stats{WorkerPackets: packets}}, nil
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "worker packets did not settle (limit 3s")
		require.Equal(t, packets, last.Stats.WorkerPackets)
		require.Equal(t, 3*time.Second, time.Since(started))
	})
}

func TestRelayLossBaselinePreservesStatusErrorAndCancellation(t *testing.T) {
	cause := errors.New("status unavailable")
	_, err := lossStableStatus(t.Context(), func(context.Context) (relay.RelayStatus, error) {
		return relay.RelayStatus{}, cause
	})
	require.ErrorIs(t, err, cause)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = lossStableStatus(ctx, func(context.Context) (relay.RelayStatus, error) {
		return relay.RelayStatus{}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}
