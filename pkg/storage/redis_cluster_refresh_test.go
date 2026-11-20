package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

// countedCluster splits the slots evenly over addrs from ClusterSlots and
// counts each load, so slot-map reloads are visible without a Redis server.
// Callers pass "localhost" names: go-redis rewrites a loopback IP
// announcement to the discovery source's host, which ClusterSlots lacks.
func countedCluster(t *testing.T, interval time.Duration, addrs ...string) (*redis.ClusterClient, *atomic.Int32) {
	t.Helper()
	var loads atomic.Int32
	cluster := RedisClusterOptions(&redis.UniversalOptions{Addrs: addrs[:1], MaxRetries: -1, MaxRedirects: -1, DialerRetries: 1})
	cluster.ClusterStateReloadInterval = interval
	cluster.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		loads.Add(1)
		var slots []redis.ClusterSlot
		for i, addr := range addrs {
			slots = append(slots, redis.ClusterSlot{Start: i * 16384 / len(addrs), End: (i+1)*16384/len(addrs) - 1, Nodes: []redis.ClusterNode{{Addr: addr}}})
		}
		return slots, nil
	}
	client := redis.NewClusterClient(cluster)
	t.Cleanup(func() { _ = client.Close() })
	return client, &loads
}

// fakeNode completes connection setup like a RESP2-only server, then passes
// each application command to handle, which returns the reply or, when hangUp
// is set, closes the connection without replying.
func fakeNode(t *testing.T, handle func(args []string) (reply string, hangUp bool)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					args, err := readCommand(reader)
					if err != nil {
						return
					}
					var reply string
					switch strings.ToLower(args[0]) {
					case "hello":
						reply = "-ERR unknown command 'HELLO'\r\n"
					case "ping":
						reply = "+PONG\r\n"
					case "auth", "select", "client":
						reply = "+OK\r\n"
					default:
						var hangUp bool
						if reply, hangUp = handle(args); hangUp {
							return
						}
					}
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	return localhostAddr(listener)
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		if _, err := reader.ReadString('\n'); err != nil {
			return nil, err
		}
		arg, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args[i] = strings.TrimSuffix(arg, "\r\n")
	}
	return args, nil
}

func localhostAddr(listener net.Listener) string {
	return net.JoinHostPort("localhost", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
}

func refreshGoroutines() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "storage.RefreshRedisClusterState.func")
}

func TestRefreshRedisClusterStateReloadsIdleClientUntilStopped(t *testing.T) {
	client, loads := countedCluster(t, 20*time.Millisecond, "localhost:16480")
	before := refreshGoroutines()
	stop := RefreshRedisClusterState(client)
	require.Equal(t, before+1, refreshGoroutines())
	// No command runs, so every load comes from the refresh.
	require.Eventually(t, func() bool { return loads.Load() >= 3 }, 5*time.Second, 5*time.Millisecond)
	stop()
	stop() // idempotent
	require.Equal(t, before, refreshGoroutines(), "stop must end the refresh goroutine")
	// A reload already in flight, plus go-redis's 200 ms coalescing cooldown,
	// may still finish after stop; after that the count must stay fixed.
	time.Sleep(500 * time.Millisecond)
	settled := loads.Load()
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, settled, loads.Load())
}

func TestRefreshRedisClusterStateReloadsAfterDialError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := localhostAddr(listener)
	require.NoError(t, listener.Close())
	client, loads := countedCluster(t, time.Hour, addr)
	defer RefreshRedisClusterState(client)()
	err = client.Set(context.Background(), "k", "v", 0).Err()
	require.True(t, IsRedisDialError(err), "%v", err)
	// One load builds the first map; the dial failure requests another.
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
}

// A node that drops connection attempts fails the dial by timeout; that is
// still a dial error, so it also requests a reload.
func TestRefreshRedisClusterStateReloadsAfterDialTimeout(t *testing.T) {
	var loads atomic.Int32
	options, err := RedisConfig{Addrs: []string{"localhost:16480"}, Cluster: true}.ConnectionOptions()
	require.NoError(t, err)
	options.MaxRetries, options.MaxRedirects = -1, -1
	var dialer net.Dialer
	options.Dialer = func(ctx context.Context, network, _ string) (net.Conn, error) {
		<-ctx.Done() // nothing answers until the dial bound
		return dialer.DialContext(ctx, network, "127.0.0.1:16480")
	}
	cluster := RedisClusterOptions(options)
	cluster.ClusterStateReloadInterval = time.Hour
	cluster.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		loads.Add(1)
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: "localhost:16480"}}}}, nil
	}
	client := redis.NewClusterClient(cluster)
	t.Cleanup(func() { _ = client.Close() })
	defer RefreshRedisClusterState(client)()
	start := time.Now()
	err = client.Set(context.Background(), "k", "v", 0).Err()
	require.True(t, IsRedisDialError(err), "%v", err)
	require.ErrorIs(t, err, context.DeadlineExceeded, "Go reports a dial timeout as a deadline too")
	require.Less(t, time.Since(start), 2*redisDialTimeout)
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
}

func TestRefreshRedisClusterStateKeepsMapAfterEstablishedConnectionError(t *testing.T) {
	var sets atomic.Int32
	addr := fakeNode(t, func(args []string) (string, bool) {
		if strings.EqualFold(args[0], "set") {
			sets.Add(1)
		}
		return "", true // the command arrived; its reply is lost
	})
	client, loads := countedCluster(t, time.Hour, addr)
	defer RefreshRedisClusterState(client)()
	err := client.Set(context.Background(), "k", "v", 0).Err()
	require.Error(t, err)
	require.False(t, IsRedisDialError(err), "%v", err)
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(1), sets.Load(), "the SET reached the node once and was not retried")
	require.Equal(t, int32(1), loads.Load(), "an established-connection error must not reload the map")
}

// A pipeline reports its first error. A refused dial to another node behind
// an ordinary command error must still request a reload.
func TestRefreshRedisClusterStateReloadsAfterPipelineDialError(t *testing.T) {
	live := fakeNode(t, func([]string) (string, bool) { return "-ERR rejected\r\n", false })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := localhostAddr(listener)
	require.NoError(t, listener.Close())
	client, loads := countedCluster(t, time.Hour, live, refused)
	defer RefreshRedisClusterState(client)()
	var liveKey, refusedKey string
	for i := 0; liveKey == "" || refusedKey == ""; i++ {
		key := fmt.Sprint("k", i)
		if redistestcluster.Slot(key) < 8192 {
			liveKey = key
		} else {
			refusedKey = key
		}
	}
	cmds, err := client.Pipelined(context.Background(), func(pipe redis.Pipeliner) error {
		pipe.Set(context.Background(), liveKey, "v", 0)
		pipe.Set(context.Background(), refusedKey, "v", 0)
		return nil
	})
	require.Error(t, err)
	require.True(t, IsRedisDialError(cmds[1].Err()), "%v", cmds[1].Err())
	require.False(t, IsRedisDialError(err), "the first error must hide the dial error: %v", err)
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
}

type tlsAlert struct{}

func (tlsAlert) Error() string { return "tls: handshake failure" }

func TestIsRedisDialError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	for _, tc := range []struct {
		err  error
		dial bool
	}{
		{refused, true},
		{fmt.Errorf("pool: %w", refused), true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, false},
		{&net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}, false},
		{&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{&net.OpError{Op: "remote error", Err: tlsAlert{}}, false},
		{&RedisEndpointPolicyError{Err: errors.New("refused")}, false},
		{errors.New("dial tcp 10.0.0.1:6379: connect: connection refused"), false},
		{nil, false},
	} {
		require.Equal(t, tc.dial, IsRedisDialError(tc.err), "%v", tc.err)
	}
}
