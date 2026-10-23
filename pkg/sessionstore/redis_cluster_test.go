package sessionstore

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestClusterRoutesIndexesAndScriptReload(t *testing.T) {
	addrs := redistestcluster.Addresses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := storage.RedisConfig{Addrs: addrs, Cluster: true, Prefix: fmt.Sprintf("cluster:%d:", time.Now().UnixNano())}
	store, err := NewRedis(ctx, cfg, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	owners, err := NewRedisOwners(ctx, cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, owners.Close()) }()
	cluster := store.client.(*redis.ClusterClient)
	require.False(t, cluster.Options().ReadOnly)
	require.False(t, cluster.Options().RouteByLatency)
	require.False(t, cluster.Options().RouteRandomly)
	worker := netip.MustParseAddrPort("127.0.0.1:23456")
	primaries := map[string]bool{}
	var leases []Lease
	var routes []Route
	// Keep creating sessions until their keys cover every primary, not merely
	// many CRC slots. The worker index itself lives on just one routed primary.
	for i := 0; len(primaries) < 3 && i < 100; i++ {
		id := fmt.Sprintf("call-%d", i)
		lease, err := store.Claim(ctx, id, worker, time.Minute)
		require.NoError(t, err)
		leases = append(leases, lease)
		node, err := cluster.MasterForKey(ctx, store.keys(id)[0])
		require.NoError(t, err)
		primaries[node.Options().Addr] = true
		route := routeFor(lease, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(30000+i)), time.Minute)
		require.NoError(t, store.PutRoute(ctx, route, true))
		routes = append(routes, route)
		require.NoError(t, store.PutState(ctx, lease, []byte(id)))
	}
	require.Len(t, primaries, 3)
	// Keep a relay tenure on each primary, so cache flush and promotion also
	// exercise the relay's distinct Lua script, independently of session scripts.
	relayPrimaries := map[string]bool{}
	var relayLeases []RelayLease
	for i := 0; len(relayPrimaries) < 3 && i < 100; i++ {
		key := fmt.Sprintf("relay-%d", i)
		node, err := cluster.MasterForKey(ctx, store.relayKey(key))
		require.NoError(t, err)
		if relayPrimaries[node.Options().Addr] {
			continue
		}
		lease, err := store.ClaimRelay(ctx, key, relayProcess("cluster-relay"), time.Minute)
		require.NoError(t, err)
		relayLeases = append(relayLeases, lease)
		relayPrimaries[node.Options().Addr] = true
	}
	require.Len(t, relayPrimaries, 3)

	check := func() {
		restored, ownerMap, err := owners.LoadRouteOwners(ctx, 100)
		require.NoError(t, err)
		require.ElementsMatch(t, routes, restored)
		require.Len(t, ownerMap, len(leases))
		listed, err := store.ListByWorker(ctx, worker)
		require.NoError(t, err)
		require.Len(t, listed, len(leases))
		for _, lease := range relayLeases {
			actual, err := store.GetRelay(ctx, lease.Key)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, actual.Epoch)
			require.Equal(t, lease.Holder, actual.Holder)
		}
		for _, lease := range leases {
			actual, err := owners.Get(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, actual.Epoch)
			state, err := store.GetState(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, []byte(lease.SessionID), state)
			_, err = store.Clock(ctx, lease.SessionID)
			require.NoError(t, err)
		}
	}
	check()
	require.NoError(t, cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error { return node.ScriptFlush(ctx).Err() }))
	// List first: this exercises pipelined EVALSHA's per-node reload, without
	// another API warming the session script on any of the affected nodes.
	listed, err := store.ListByWorker(ctx, worker)
	require.NoError(t, err)
	require.Len(t, listed, len(leases))
	for _, lease := range relayLeases {
		renewed, err := store.RenewRelay(ctx, lease, time.Minute)
		require.NoError(t, err)
		require.Equal(t, lease.Epoch, renewed.Epoch)
	}
	check()
	admin := redis.NewClusterClient(storage.RedisClusterOptions(&redis.UniversalOptions{Addrs: addrs, MaxRetries: -1}))
	defer func() { _ = admin.Close() }()
	require.NoError(t, redistestcluster.Failover(ctx, admin, store.keys(leases[0].SessionID)[0]))
	require.Eventually(t, func() bool {
		got, e := owners.Get(ctx, leases[0].SessionID)
		return e == nil && got.Epoch == leases[0].Epoch
	}, 3*time.Second, 20*time.Millisecond)
	check()
	next, err := store.Transfer(ctx, leases[0], netip.MustParseAddrPort("127.0.0.1:23457"), time.Minute)
	require.NoError(t, err)
	require.Greater(t, next.Epoch, leases[0].Epoch)
	require.ErrorIs(t, store.PutState(ctx, leases[0], []byte("stale")), ErrLeaseLost)
	require.NoError(t, store.Release(ctx, leases[0]))
	got, err := store.Get(ctx, next.SessionID)
	require.NoError(t, err)
	require.Equal(t, next.Epoch, got.Epoch)
}
