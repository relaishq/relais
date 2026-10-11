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

func TestRelayRenewRecreatesMissingLease(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		store := newStore(t)
		s := store.(RelayLeases)
		ctx := context.Background()
		a, err := s.ClaimRelay(ctx, "public", relayProcess("a"), time.Second)
		require.NoError(t, err)
		a, err = s.ActivateRelay(ctx, a)
		require.NoError(t, err)
		switch s := store.(type) {
		case *Memory:
			s.mu.Lock()
			delete(s.relayLeases, a.Key)
			s.mu.Unlock()
		case *Redis:
			keys, err := s.client.Keys(ctx, s.prefix+"relay:*").Result()
			require.NoError(t, err)
			require.Len(t, keys, 1)
			require.NoError(t, s.client.Del(ctx, keys[0]).Err())
		}
		restored, err := s.RenewRelay(ctx, a, time.Second)
		require.NoError(t, err, "store loss without a successor must not self-fence")
		require.Equal(t, a.Holder, restored.Holder)
		require.Equal(t, a.Holder, restored.Forwarder)
		require.GreaterOrEqual(t, restored.Epoch, a.Epoch)
		_, err = s.ClaimRelay(ctx, a.Key, relayProcess("b"), time.Second)
		require.ErrorIs(t, err, ErrLeaseHeld)
	})
}
func TestRelayLeaseAllowsPIDOne(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		s := newStore(t).(RelayLeases)
		p := relayProcess("container")
		p.PID = 1
		_, err := s.ClaimRelay(context.Background(), "container", p, time.Second)
		require.NoError(t, err)
	})
}

func TestRelayReleaseRetainsEpochAndRejectsStaleRelease(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		s := newStore(t).(RelayLeases)
		ctx := context.Background()
		a, err := s.ClaimRelay(ctx, "one", relayProcess("a"), time.Second)
		require.NoError(t, err)
		_, err = s.ActivateRelay(ctx, a)
		require.NoError(t, err)
		require.NoError(t, s.ReleaseRelay(ctx, a))
		released, err := s.GetRelay(ctx, a.Key)
		require.NoError(t, err)
		require.True(t, released.Expired)
		require.Empty(t, released.Holder.Owner)
		require.Empty(t, released.Forwarder.Owner)
		require.Equal(t, a.Epoch, released.Epoch)
		b, err := s.ClaimRelay(ctx, a.Key, relayProcess("b"), time.Second)
		require.NoError(t, err)
		require.Greater(t, b.Epoch, a.Epoch)
		require.Empty(t, b.PreviousHolder.Owner)
		require.ErrorIs(t, s.ReleaseRelay(ctx, a), ErrLeaseLost)
		current, err := s.GetRelay(ctx, a.Key)
		require.NoError(t, err)
		require.Equal(t, b.Holder, current.Holder)
	})
}

func TestRelayRenewRepairsOlderEpochButRejectsNewerEpoch(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(*testing.T) Store) {
		store := newStore(t)
		s := store.(RelayLeases)
		ctx := context.Background()
		previous, err := s.ClaimRelay(ctx, "one", relayProcess("previous"), time.Second)
		require.NoError(t, err)
		a, err := s.TransferRelay(ctx, previous, relayProcess("a"), time.Second)
		require.NoError(t, err)
		_, err = s.ActivateRelay(ctx, a)
		require.NoError(t, err)
		setEpoch := func(epoch uint64) {
			switch store := store.(type) {
			case *Memory:
				store.mu.Lock()
				l := store.relayLeases[a.Key]
				l.Epoch = epoch
				store.relayLeases[a.Key] = l
				store.mu.Unlock()
			case *Redis:
				keys, err := store.client.Keys(ctx, store.prefix+"relay:*").Result()
				require.NoError(t, err)
				require.Len(t, keys, 1)
				require.NoError(t, store.client.HSet(ctx, keys[0], "epoch", epoch).Err())
			}
		}
		setEpoch(a.Epoch - 1)
		renewed, err := s.RenewRelay(ctx, a, time.Second)
		require.NoError(t, err)
		require.Equal(t, a.Epoch, renewed.Epoch)
		setEpoch(a.Epoch + 1)
		_, err = s.RenewRelay(ctx, a, time.Second)
		require.ErrorIs(t, err, ErrLeaseLost)
	})
}
