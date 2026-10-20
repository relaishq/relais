package sessionstore

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComponentExpiryPruneAndFreshClaim(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cs := s.(ComponentStore)
		worker := netip.MustParseAddrPort("127.0.0.1:23456")
		for _, prune := range []string{"get", "state", "checkpoint", "index"} {
			t.Run(prune, func(t *testing.T) {
				id := "expire-" + prune
				lease, err := s.Claim(ctx, id, worker, time.Minute)
				require.NoError(t, err)
				tuple, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
				require.NoError(t, err)
				// Deterministically expire the lease while retaining banks, emulating an
				// expiry/prune race without depending on subsecond callback timing.
				if m, ok := s.(*Memory); ok {
					expired := m.leases[id]
					expired.ExpiresAt = time.Now().Add(-time.Minute)
					m.leases[id] = expired
				} else {
					r := s.(*Redis)
					require.NoError(t, r.client.Del(ctx, r.keys(id)[0]).Err())
				}
				switch prune {
				case "get":
					_, err = s.Get(ctx, id)
					require.ErrorIs(t, err, ErrNotFound)
				case "state":
					_, err = s.GetState(ctx, id)
					require.ErrorIs(t, err, ErrNotFound)
				case "checkpoint":
					_, err = s.Checkpoint(ctx, id, nil)
					require.ErrorIs(t, err, ErrNotFound)
				case "index":
					_, err = s.ListByWorker(ctx, worker)
					require.NoError(t, err)
				}
				if r, ok := s.(*Redis); ok {
					for _, key := range r.componentKeys(id)[5:] {
						require.Zero(t, r.client.Exists(ctx, key).Val())
					}
				} else {
					require.NotContains(t, s.(*Memory).componentRecords, id)
				}
				reclaimed, err := s.Claim(ctx, id, worker, time.Minute)
				require.NoError(t, err)
				require.NoError(t, s.Release(ctx, lease))
				next, err := cs.CommitComponents(ctx, reclaimed, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
				require.NoError(t, err)
				require.NotEqual(t, tuple.Metadata.SetupGeneration, next.Metadata.SetupGeneration)
				require.NoError(t, s.Release(ctx, reclaimed))
			})
		}
	})
}
func TestComponentRenewDoesNotRecreateMissingBank(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "missing-bank", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		key := r.componentKeys(lease.SessionID)[9+int(first.Metadata.Banks[2])]
		require.NoError(t, r.client.Del(ctx, key).Err())
		next, err := r.Renew(ctx, lease, 2*time.Minute)
		require.NoError(t, err)
		next, err = r.Transfer(ctx, next, netip.MustParseAddrPort("127.0.0.1:23457"), 3*time.Minute)
		require.NoError(t, err)
		require.Zero(t, r.client.Exists(ctx, key).Val())
		_, err = r.ReadComponents(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrUnsafeCheckpoint)
		_, err = r.CommitComponents(ctx, next, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1, Expected: &first.Metadata})
		require.ErrorIs(t, err, ErrUnsafeCheckpoint)
	})
}
func TestComponentFreshClaimRemovesOrphanBanks(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		id := "orphan"
		keys := r.componentKeys(id)
		// Orphan preparations without ownership cannot become a restore fallback.
		require.NoError(t, r.client.HSet(ctx, keys[5], "0", "orphan", "1", "orphan").Err())
		require.NoError(t, r.client.HSet(ctx, keys[6], "m", "orphan").Err())
		for _, key := range keys[7:] {
			require.NoError(t, r.client.Set(ctx, key, "orphan", 0).Err())
		}
		lease, err := r.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		for _, key := range keys[5:] {
			require.Zero(t, r.client.Exists(ctx, key).Val(), fmt.Sprint(key))
		}
		require.NoError(t, r.Release(ctx, lease))
	})
}
