package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// countedCluster serves a fixed one-node slot map from ClusterSlots and counts
// each load, so slot-map reloads are visible without a Redis server. Callers
// pass a "localhost" name: go-redis rewrites a loopback IP announcement to the
// discovery source's host, which a custom ClusterSlots does not have.
func countedCluster(t *testing.T, addr string, interval time.Duration) (*redis.ClusterClient, *atomic.Int32) {
	t.Helper()
	var loads atomic.Int32
	cluster := RedisClusterOptions(&redis.UniversalOptions{Addrs: []string{addr}, MaxRetries: -1, MaxRedirects: -1, DialerRetries: 1})
	cluster.ClusterStateReloadInterval = interval
	cluster.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		loads.Add(1)
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: addr}}}}, nil
	}
	client := redis.NewClusterClient(cluster)
	t.Cleanup(func() { _ = client.Close() })
	return client, &loads
}

func localhostAddr(listener net.Listener) string {
	return net.JoinHostPort("localhost", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
}

func refreshGoroutines() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "storage.RefreshRedisClusterState.func")
}

func TestRefreshRedisClusterStateReloadsIdleClientUntilStopped(t *testing.T) {
	client, loads := countedCluster(t, "localhost:16480", 20*time.Millisecond)
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
	client, loads := countedCluster(t, addr, time.Hour)
	defer RefreshRedisClusterState(client)()
	err = client.Set(context.Background(), "k", "v", 0).Err()
	require.True(t, IsRedisDialError(err), "%v", err)
	// One load builds the first map; the dial failure requests another.
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
}

func TestRefreshRedisClusterStateKeepsMapAfterEstablishedConnectionError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close() // the command was sent; the reply is lost
		}
	}()
	client, loads := countedCluster(t, localhostAddr(listener), time.Hour)
	defer RefreshRedisClusterState(client)()
	err = client.Set(context.Background(), "k", "v", 0).Err()
	require.Error(t, err)
	var opError *net.OpError
	require.True(t, errors.Is(err, io.EOF) || errors.As(err, &opError) && opError.Op == "read", "%v", err)
	require.False(t, IsRedisDialError(err), "%v", err)
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(1), loads.Load())
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
