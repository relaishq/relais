package sessionstore

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// Issue #517: an idle client kept its pre-failover slot map. go-redis reloads
// the map only when a command finds it stale, and that command still uses the
// old map, so the first write went to the dead primary and failed at once.
func TestClusterIdleClientWritesAfterPrimaryFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("Redis Cluster integration is excluded from short mode")
	}
	if os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("set RELAIS_TEST_REDIS_CLUSTER=1 to start a disposable local cluster")
	}
	// Always own the cluster: this test shuts a primary down for good.
	addrs := redistestcluster.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := storage.RedisConfig{Addrs: addrs, Cluster: true, Prefix: fmt.Sprintf("idle-failover:%d:", time.Now().UnixNano())}
	store, err := NewRedis(ctx, cfg, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	options, err := cfg.ConnectionOptions()
	require.NoError(t, err)
	options.MaxRetries = -1
	admin := redis.NewClusterClient(storage.RedisClusterOptions(options))
	defer func() { _ = admin.Close() }()
	cluster := store.client.(*redis.ClusterClient)
	require.NoError(t, redistestcluster.WaitForStable(ctx, admin, cluster))

	lease, err := store.Claim(ctx, "idle", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	primary, err := admin.MasterForKey(ctx, store.keys("idle")[0])
	require.NoError(t, err)
	id, err := primary.Do(ctx, "CLUSTER", "MYID").Text()
	require.NoError(t, err)
	rows, err := primary.Do(ctx, "CLUSTER", "REPLICAS", id).StringSlice()
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	fields := strings.Fields(rows[0])
	require.GreaterOrEqual(t, len(fields), 2)
	replicaAddr, _, _ := strings.Cut(fields[1], "@")
	replicaAddr, _, _ = strings.Cut(replicaAddr, ",")
	require.NoError(t, redisendpoint.ValidateDev(replicaAddr))
	replicaOptions := *primary.Options()
	replicaOptions.Addr = replicaAddr
	replica := redis.NewClient(&replicaOptions)
	defer func() { _ = replica.Close() }()
	acked, err := primary.Wait(ctx, 1, 3*time.Second).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)

	// The primary never comes back; its address now refuses connections.
	_ = primary.Do(ctx, "SHUTDOWN", "NOSAVE").Err()
	require.NoError(t, replica.Do(ctx, "CLUSTER", "FAILOVER", "TAKEOVER").Err())
	require.Eventually(t, func() bool {
		info, err := replica.Info(ctx, "replication").Result()
		return err == nil && strings.Contains(info, "role:master")
	}, 10*time.Second, 10*time.Millisecond)

	// Stay idle past the reload interval, so a lazy reload started by the next
	// command would come too late for that command.
	time.Sleep(cluster.Options().ClusterStateReloadInterval + 2*time.Second)
	renewed, err := store.Renew(ctx, lease, time.Minute)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, renewed.Epoch)
}
