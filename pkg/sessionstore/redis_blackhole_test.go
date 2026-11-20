package sessionstore

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// blackholeDial dials every address normally except target, which behaves
// like a host that drops connection attempts: nothing answers until the dial
// deadline, and then the dial fails as Go reports it ("i/o timeout").
func blackholeDial(target string, dials *atomic.Int32) func(context.Context, string, string) (net.Conn, error) {
	var dialer net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != target {
			return dialer.DialContext(ctx, network, addr)
		}
		dials.Add(1)
		<-ctx.Done()
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// The expired context makes Go fail the connect before it sends
		// anything: "dial tcp 127.0.0.1:<port>: i/o timeout".
		return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
}

// A real dial that times out matches the dial kind, not the canceled kind:
// Go's dial timeout error also matches context.DeadlineExceeded.
func TestRedisKindOfTimedOutDial(t *testing.T) {
	var dials atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := blackholeDial("localhost:1", &dials)(ctx, "tcp", "localhost:1")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, storage.IsRedisDialError(err), "%v", err)
	require.Equal(t, "dial", redisKind(err))
	// A command that outlives its own deadline still sent nothing that proves
	// it did not run; that stays the canceled kind.
	require.Equal(t, "canceled", redisKind(context.DeadlineExceeded))
	require.Equal(t, "canceled", redisKind(errors.Join(errors.New("wait for connection"), context.DeadlineExceeded)))
}

// Issue #517 B8: after a store failover the old primary stopped answering
// connection attempts instead of refusing them. An idle client whose slot map
// still named it sent its first write there. go-redis dials in a goroutine
// detached from the command, so with its five-second default the write waited
// out the whole two-second command limit and failed with the command's own
// deadline: no dial error, so no slot-map reload and no retry. The dial must
// fail inside the limit, and the write must retry on the refreshed map.
func TestRedisWriteSurvivesBlackholedOldPrimary(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	oldPrimary := net.JoinHostPort("localhost", strconv.Itoa(dead.Addr().(*net.TCPAddr).Port))
	require.NoError(t, dead.Close())
	newPrimary, scripts := fakeRenewPrimary(t)

	// Product connection options, including their dial bound; only the
	// transport to the old primary is replaced.
	options, err := storage.RedisConfig{Addrs: []string{oldPrimary}, Cluster: true}.ConnectionOptions()
	require.NoError(t, err)
	var blackholed atomic.Int32
	options.Dialer = blackholeDial(oldPrimary, &blackholed)
	options.MaxRetries, options.MaxRedirects = -1, -1
	cluster := storage.RedisClusterOptions(options)
	cluster.ClusterStateReloadInterval = time.Hour // only a failed dial asks for a reload
	var loads atomic.Int32
	cluster.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		addr := oldPrimary
		if loads.Add(1) > 1 {
			addr = newPrimary
		}
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: addr}}}}, nil
	}
	client := redis.NewClusterClient(cluster)
	stop := storage.RefreshRedisClusterState(client)
	defer func() { stop(); _ = client.Close() }()
	r := &Redis{client: client, prefix: "blackhole:", retention: time.Minute}

	start := time.Now()
	result, err := r.run(context.Background(), "renew", "id", "127.0.0.1:1", "1", time.Minute)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, result, 4)
	require.GreaterOrEqual(t, blackholed.Load(), int32(1), "the write first tried the old primary")
	require.Equal(t, int32(1), scripts.Load(), "the new primary runs the write exactly once")
	require.Less(t, elapsed, transitionCommandLimit*3/4, "the write needs room left for its caller")
}
