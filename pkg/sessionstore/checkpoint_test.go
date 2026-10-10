package sessionstore

import (
	"context"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckpointClockFencingAndIdentity(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		a, b := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
		lease, err := store.Claim(ctx, "checkpoint", a, time.Minute)
		require.NoError(t, err)
		before, err := store.Clock(ctx)
		require.NoError(t, err)
		original := []byte("original")
		require.NoError(t, store.PutState(ctx, lease, original))
		first, err := store.Checkpoint(ctx, lease.SessionID, original)
		require.NoError(t, err)
		require.False(t, first.StoredAt.Before(before))
		require.False(t, first.Now.Before(first.StoredAt))
		time.Sleep(15 * time.Millisecond)
		next, err := store.Transfer(ctx, lease, b, time.Minute)
		require.NoError(t, err)
		require.ErrorIs(t, store.PutState(ctx, lease, []byte("stale")), ErrLeaseLost)
		second, err := store.Checkpoint(ctx, lease.SessionID, original)
		require.NoError(t, err)
		require.True(t, first.StoredAt.Equal(second.StoredAt), "transfer and failed put never refresh a checkpoint")
		require.GreaterOrEqual(t, second.Age, 10*time.Millisecond)
		require.NoError(t, store.PutState(ctx, next, []byte("newer")))
		_, err = store.Checkpoint(ctx, lease.SessionID, original)
		require.ErrorIs(t, err, ErrStateSuperseded, "newer checkpoint cannot date older bytes")
		require.NoError(t, store.Release(ctx, next))
		_, err = store.Checkpoint(ctx, lease.SessionID, []byte("newer"))
		require.ErrorIs(t, err, ErrNotFound)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = store.Clock(canceled)
		require.ErrorIs(t, err, context.Canceled)
		_, err = store.Checkpoint(canceled, lease.SessionID, original)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestCheckpointClockRegressionIsUnsafe(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		lease, err := store.Claim(ctx, "clock", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
		require.NoError(t, err)
		data := []byte("checkpoint")
		require.NoError(t, store.PutState(ctx, lease, data))
		future := time.Now().Add(time.Hour)
		switch s := store.(type) {
		case *Memory:
			s.mu.Lock()
			checkpoint := s.checkpoints[lease.SessionID]
			checkpoint.StoredAt = future
			s.checkpoints[lease.SessionID] = checkpoint
			s.mu.Unlock()
		case *Redis:
			require.NoError(t, s.client.HSet(ctx, s.keys(lease.SessionID)[0], "checkpoint_at", strconv.FormatInt(future.UnixMilli(), 10)).Err())
		}
		_, err = store.Checkpoint(ctx, lease.SessionID, data)
		require.ErrorIs(t, err, ErrUnsafeCheckpointClock)
	})
}
