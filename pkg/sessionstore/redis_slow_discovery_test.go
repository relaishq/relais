package sessionstore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// readRESPCommand reads one client command as an array of bulk strings.
func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected RESP line %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		if _, err := reader.ReadString('\n'); err != nil { // $<len>
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

// A new primary that answers every session script with a renewed lease.
func fakeRenewPrimary(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var scripts atomic.Int32
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
					args, err := readRESPCommand(reader)
					if err != nil {
						return
					}
					reply := "+OK\r\n"
					switch strings.ToLower(args[0]) {
					case "hello": // answer like a RESP2-only server
						reply = "-ERR unknown command 'HELLO'\r\n"
					case "evalsha", "eval":
						scripts.Add(1)
						expires := strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10)
						reply = fmt.Sprintf("*4\r\n:1\r\n$11\r\n127.0.0.1:1\r\n$1\r\n1\r\n$%d\r\n%s\r\n", len(expires), expires)
					}
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	return net.JoinHostPort("localhost", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)), &scripts
}

// Issue #517: a failover between refresh ticks leaves the old primary in the
// map, and discovery of the new map takes longer than three quick refused
// dials. The write must keep retrying the refused dial inside its two-second
// limit until the refreshed map routes it to the new primary.
func TestRedisWriteOutlastsSlowSlotDiscovery(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	// "localhost" keeps go-redis from rewriting a loopback announcement.
	oldPrimary := net.JoinHostPort("localhost", strconv.Itoa(dead.Addr().(*net.TCPAddr).Port))
	require.NoError(t, dead.Close())
	newPrimary, scripts := fakeRenewPrimary(t)

	const discovery = 400 * time.Millisecond
	var loads atomic.Int32
	cluster := storage.RedisClusterOptions(&redis.UniversalOptions{Addrs: []string{oldPrimary}, MaxRetries: -1, MaxRedirects: -1, DialerRetries: 1, Protocol: 2, DisableIdentity: true})
	cluster.ClusterStateReloadInterval = time.Hour // only a failed dial asks for a reload
	cluster.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		addr := oldPrimary
		if loads.Add(1) > 1 {
			time.Sleep(discovery)
			addr = newPrimary
		}
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: addr}}}}, nil
	}
	client := redis.NewClusterClient(cluster)
	stop := storage.RefreshRedisClusterState(client)
	defer func() { stop(); _ = client.Close() }()
	r := &Redis{client: client, prefix: "slow-discovery:", retention: time.Minute}

	start := time.Now()
	result, err := r.run(context.Background(), "renew", "id", "127.0.0.1:1", "1", time.Minute)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, result, 4)
	require.Equal(t, int32(1), scripts.Load(), "the new primary runs the write exactly once")
	require.GreaterOrEqual(t, elapsed, discovery)
	require.Less(t, elapsed, transitionCommandLimit)
}
