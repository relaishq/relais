package sessionstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReleaseTombstoneDistinguishesEndFromExpiry(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		a, b := netip.MustParseAddrPort("127.0.0.1:4001"), netip.MustParseAddrPort("127.0.0.1:4002")
		lease, err := store.Claim(ctx, "ended", a, time.Minute)
		require.NoError(t, err)
		require.NoError(t, store.PutState(ctx, lease, []byte("snapshot")))
		require.NoError(t, store.Release(ctx, lease))
		// Repeated authoritative reads/pruning must retain the marker, separately
		// from record and snapshot metadata, until its five-minute deadline.
		for range 3 {
			ended, err := store.Get(ctx, "ended")
			require.ErrorIs(t, err, ErrEnded)
			require.Equal(t, lease.Worker, ended.Worker)
			require.Equal(t, lease.Epoch, ended.Epoch)
			require.True(t, ended.ExpiresAt.After(time.Now().Add(4*time.Minute)))
			_, err = store.GetState(ctx, "ended")
			require.ErrorIs(t, err, ErrNotFound)
		}
		next, err := store.Claim(ctx, "ended", b, time.Minute)
		require.NoError(t, err)
		next, err = store.Transfer(ctx, next, a, time.Minute)
		require.NoError(t, err)
		require.NoError(t, store.PutState(ctx, next, []byte("successor")))
		require.NoError(t, store.Release(ctx, lease))
		current, err := store.Get(ctx, "ended")
		require.NoError(t, err)
		require.Equal(t, next, current)
		blob, err := store.GetState(ctx, "ended")
		require.NoError(t, err)
		require.Equal(t, "successor", string(blob))
		_, err = store.Renew(ctx, next, time.Millisecond)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		// A fenced worker's delayed Release must not turn real expiry into an end.
		require.NoError(t, store.Release(ctx, next))
		_, err = store.Get(ctx, "ended")
		require.ErrorIs(t, err, ErrNotFound)
	})
}
