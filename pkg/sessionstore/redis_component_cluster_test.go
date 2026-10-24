package sessionstore

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

func TestComponentClusterInventoryAndScriptReload(t *testing.T) {
	addrs := redistestcluster.Addresses(t)
	r := componentCluster(t, addrs)
	ctx := context.Background()
	cluster := r.client.(*redis.ClusterClient)
	primaries := map[string]bool{}
	tuples := map[string]ComponentTuple{}
	leases := map[string]Lease{}
	for i := 0; len(primaries) < 3 && i < 100; i++ {
		id := fmt.Sprintf("inventory-%d", i)
		lease, err := r.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		tuple, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(id), StateVersion: 1})
		require.NoError(t, err)
		tuples[id] = tuple
		leases[id] = lease
		primary, err := cluster.MasterForKey(ctx, r.componentKeys(id)[0])
		require.NoError(t, err)
		primaries[primary.Options().Addr] = true
	}
	require.Len(t, primaries, 3)
	inventory, err := r.ComponentKeyInventory(ctx)
	require.NoError(t, err)
	require.Equal(t, map[byte]uint64{0: uint64(5 * len(leases))}, inventory)
	require.NoError(t, cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error { return node.ScriptFlush(ctx).Err() }))
	// Read first to prove the tuple script reloads on each primary.
	for id, tuple := range tuples {
		restored, err := r.ReadComponents(ctx, id)
		require.NoError(t, err)
		require.Equal(t, tuple.Metadata, restored.Metadata)
	}
	require.NoError(t, cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error { return node.ScriptFlush(ctx).Err() }))
	// Commit independently reloads its script after NOSCRIPT on the slot's
	// actual primary, without a preliminary component read.
	for id, tuple := range tuples {
		next, err := r.CommitComponents(ctx, leases[id], ComponentCommit{Components: tuple.Components, StateVersion: 1, Expected: &tuple.Metadata})
		require.NoError(t, err)
		require.Greater(t, next.Metadata.References[4].Revision, tuple.Metadata.References[4].Revision)
	}
}

// Redis 7 legacy migration deliberately splits a session's declared keys
// across importing/migrating nodes. TRYAGAIN is a bounded liveness stall, never
// permission to publish a torn checkpoint or relax the owner/root fences.
func TestComponentClusterLegacySlotMigration(t *testing.T) {
	addrs := redistestcluster.Addresses(t)
	r := componentCluster(t, addrs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cluster := r.client.(*redis.ClusterClient)
	lease, err := r.Claim(ctx, "migration", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	first, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
	require.NoError(t, err)
	keys := r.componentKeys(lease.SessionID)
	source, err := cluster.MasterForKey(ctx, keys[0])
	require.NoError(t, err)
	var target *redis.Client
	var lock sync.Mutex
	require.NoError(t, cluster.ForEachMaster(ctx, func(_ context.Context, node *redis.Client) error {
		if node.Options().Addr != source.Options().Addr {
			lock.Lock()
			if target == nil {
				target = node
			}
			lock.Unlock()
		}
		return nil
	}))
	require.NotNil(t, target)
	slot, err := source.ClusterKeySlot(ctx, keys[0]).Result()
	require.NoError(t, err)
	sourceID, err := source.Do(ctx, "CLUSTER", "MYID").Text()
	require.NoError(t, err)
	targetID, err := target.Do(ctx, "CLUSTER", "MYID").Text()
	require.NoError(t, err)
	require.NoError(t, target.Do(ctx, "CLUSTER", "SETSLOT", slot, "IMPORTING", sourceID).Err())
	require.NoError(t, source.Do(ctx, "CLUSTER", "SETSLOT", slot, "MIGRATING", targetID).Err())
	// Install cleanup before any assertions that could abort an open migration.
	t.Cleanup(func() {
		cleanup := context.Background()
		_ = source.Do(cleanup, "CLUSTER", "SETSLOT", slot, "STABLE").Err()
		_ = target.Do(cleanup, "CLUSTER", "SETSLOT", slot, "STABLE").Err()
	})
	host, port, err := net.SplitHostPort(target.Options().Addr)
	require.NoError(t, err)
	migrate := func(key string) {
		require.NoError(t, source.Do(ctx, "MIGRATE", host, port, key, 0, 2000, "REPLACE").Err())
	}
	// Move a single precreated inactive bank. The commit now spans two nodes.
	migrate(keys[8])
	started := time.Now()
	_, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: first.Components, StateVersion: 1, Expected: &first.Metadata})
	require.ErrorIs(t, err, ErrTransient)
	require.Less(t, time.Since(started), 3*time.Second)
	// Complete this slot's migration, including the metadata keys which exist.
	for _, key := range keys {
		if key == keys[8] {
			continue
		}
		if source.Exists(ctx, key).Val() == 1 {
			migrate(key)
		}
	}
	require.NoError(t, target.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetID).Err())
	require.NoError(t, source.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetID).Err())
	require.NoError(t, cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		return node.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetID).Err()
	}))
	require.NoError(t, redistestcluster.WaitForStable(ctx, cluster))
	restored, err := r.ReadComponents(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, first.Metadata, restored.Metadata)
	require.Equal(t, first.Components, restored.Components)
	next, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: first.Components, StateVersion: 1, Expected: &first.Metadata})
	require.NoError(t, err)
	stale := lease
	stale.Epoch--
	_, err = r.CommitComponents(ctx, stale, ComponentCommit{Components: next.Components, StateVersion: 1, Expected: &next.Metadata})
	require.ErrorIs(t, err, ErrLeaseLost)
	t.Logf("Redis legacy slot migration: split-key write stalled within %s; tuple consistent; post-migration write and fencing passed", time.Since(started))
}

func TestComponentInventoryIncludesLegacyRotation(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		lease, err := r.Claim(ctx, "legacy-key-inventory", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		require.NoError(t, r.PutState(ctx, lease, []byte("legacy snapshot")))
		counts, err := r.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{0: 1}, counts)
		r.keysByID[1] = make([]byte, 32)
		r.keysByID[1][0] = 1
		r.activeKey = 1
		counts, err = r.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{0: 1}, counts, "legacy snapshots also prevent premature key retirement")
		require.NoError(t, r.PutState(ctx, lease, []byte("legacy snapshot")))
		counts, err = r.ComponentKeyInventory(ctx)
		require.NoError(t, err)
		require.Equal(t, map[byte]uint64{1: 1}, counts)
	})
}
