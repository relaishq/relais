package sessionstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func relayProcess(owner string) RelayProcess {
	return RelayProcess{Owner: owner, PID: 42, Start: "kernel-start"}
}
func TestRelayLeaseLifecycle(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		s := newStore(t).(RelayLeases)
		ctx := context.Background()
		ttl := 50 * time.Millisecond
		a, err := s.ClaimRelay(ctx, "public", relayProcess("a"), ttl)
		require.NoError(t, err)
		same, err := s.ClaimRelay(ctx, "public", a.Holder, ttl)
		require.NoError(t, err)
		require.Equal(t, a.Epoch, same.Epoch)
		_, err = s.ClaimRelay(ctx, "public", relayProcess("b"), ttl)
		require.ErrorIs(t, err, ErrLeaseHeld)
		a, err = s.ActivateRelay(ctx, a)
		require.NoError(t, err)
		require.Equal(t, a.Holder, a.Forwarder)
		time.Sleep(ttl + 10*time.Millisecond)
		// Expiry does not erase the process identity or by itself stop forwarding.
		expired, err := s.GetRelay(ctx, "public")
		require.NoError(t, err)
		require.Equal(t, a.Forwarder, expired.Forwarder)
		a, err = s.RenewRelay(ctx, a, ttl)
		require.NoError(t, err)
		time.Sleep(ttl + 10*time.Millisecond)
		b, err := s.ClaimRelay(ctx, "public", relayProcess("b"), ttl)
		require.NoError(t, err)
		require.Greater(t, b.Epoch, a.Epoch)
		require.Equal(t, a.Holder, b.PreviousHolder)
		require.Equal(t, a.Holder, b.Forwarder)
		_, err = s.RenewRelay(ctx, a, ttl)
		require.ErrorIs(t, err, ErrLeaseLost)
		_, err = s.ActivateRelay(ctx, a)
		require.ErrorIs(t, err, ErrLeaseLost)
		// A claimant crashes before fencing: a later claimant must still fence A.
		time.Sleep(ttl + 10*time.Millisecond)
		c, err := s.ClaimRelay(ctx, "public", relayProcess("c"), ttl)
		require.NoError(t, err)
		require.Equal(t, a.Holder, c.Forwarder)
		require.Equal(t, b.Holder, c.PreviousHolder)
		c, err = s.ActivateRelay(ctx, c)
		require.NoError(t, err)
		require.Equal(t, c.Holder, c.Forwarder)
		require.Empty(t, c.PreviousHolder.Owner)
		d, err := s.TransferRelay(ctx, c, relayProcess("d"), time.Second)
		require.NoError(t, err)
		require.Greater(t, d.Epoch, c.Epoch)
		require.Equal(t, c.Holder, d.Forwarder)
		_, err = s.TransferRelay(ctx, c, relayProcess("e"), ttl)
		require.ErrorIs(t, err, ErrLeaseLost)
	})
}
func TestRelayLeaseConcurrentClaims(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		s := newStore(t).(RelayLeases)
		var winners atomic.Int64
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				p := relayProcess(string(rune('a' + i)))
				_, err := s.ClaimRelay(context.Background(), "one", p, time.Second)
				if err == nil {
					winners.Add(1)
				} else {
					require.ErrorIs(t, err, ErrLeaseHeld)
				}
			})
		}
		wg.Wait()
		require.EqualValues(t, 1, winners.Load())
	})
}
func TestRelayLeaseExpiredActivationAndTransfer(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		s := newStore(t).(RelayLeases)
		ctx := context.Background()
		a, err := s.ClaimRelay(ctx, "one", relayProcess("a"), 10*time.Millisecond)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
		_, err = s.ActivateRelay(ctx, a)
		require.ErrorIs(t, err, ErrLeaseLost)
		_, err = s.TransferRelay(ctx, a, relayProcess("b"), time.Second)
		require.ErrorIs(t, err, ErrLeaseLost)
		_, err = s.GetRelay(ctx, "missing")
		require.ErrorIs(t, err, ErrNotFound)
	})
}
