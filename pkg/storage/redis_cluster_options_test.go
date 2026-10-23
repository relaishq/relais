package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/stretchr/testify/require"
)

// All logical addresses use net.Pipe. Even deliberately broken policy variants
// cannot open sockets to the reserved ports or to the documentation-only hosts.
type clusterWire struct {
	mu      sync.Mutex
	dials   map[string]int
	sources []string
	slots   func(string, int) string
}

func (w *clusterWire) dial(_ context.Context, _, addr string) (net.Conn, error) {
	w.mu.Lock()
	w.dials[addr]++
	w.mu.Unlock()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_ = server.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(server)
		for {
			args, err := readClusterWireCommand(reader)
			if err != nil {
				return
			}
			response := "+OK\r\n"
			switch strings.ToLower(args[0]) {
			case "hello":
				response = "-ERR unknown command HELLO\r\n"
			case "ping":
				response = "+PONG\r\n"
			case "command":
				response = "*0\r\n"
			case "cluster":
				w.mu.Lock()
				w.sources = append(w.sources, addr)
				response = w.slots(addr, len(w.sources))
				w.mu.Unlock()
			}
			if _, err := io.WriteString(server, response); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func readClusterWireCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil {
			return nil, err
		}
		data := make([]byte, size+2)
		if _, err = io.ReadFull(r, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}

func slotWire(start, end int, host *string, port int) string {
	raw := "$-1\r\n"
	if host != nil {
		raw = fmt.Sprintf("$%d\r\n%s\r\n", len(*host), *host)
	}
	return fmt.Sprintf("*3\r\n:%d\r\n:%d\r\n*4\r\n%s:%d\r\n$4\r\nnode\r\n*2\r\n$8\r\nhostname\r\n$5\r\ncache\r\n", start, end, raw, port)
}
func wireHost(s string) *string { return &s }

func wireCluster(t *testing.T, seeds []string, slots func(string, int) string, validate func(string) error) (*redis.ClusterClient, *clusterWire) {
	t.Helper()
	wire := &clusterWire{dials: make(map[string]int), slots: slots}
	options := redisClusterOptions(&redis.UniversalOptions{Addrs: seeds, Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, MaxRetries: -1, MaxRedirects: -1, Dialer: wire.dial}, validate)
	require.False(t, options.ReadOnly)
	require.False(t, options.RouteByLatency)
	require.False(t, options.RouteRandomly)
	client := redis.NewClusterClient(options)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client, wire
}

func TestRedisClusterDistinctLoopbackNodes(t *testing.T) {
	slots := "*2\r\n" + slotWire(0, 8191, wireHost("127.0.0.1"), 16380) + slotWire(8192, 16383, wireHost("127.0.0.2"), 16380)
	client, _ := wireCluster(t, []string{"127.0.0.1:16381"}, func(string, int) string { return slots }, redisendpoint.Validate)
	var mu sync.Mutex
	var addrs []string
	require.NoError(t, client.ForEachMaster(context.Background(), func(ctx context.Context, node *redis.Client) error {
		mu.Lock()
		addrs = append(addrs, node.Options().Addr)
		mu.Unlock()
		return node.Ping(ctx).Err()
	}))
	require.ElementsMatch(t, []string{"127.0.0.1:16380", "127.0.0.2:16380"}, addrs)
}

func TestRedisClusterEffectiveEndpointPolicy(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	for _, tc := range []struct {
		name, source string
		host         *string
		port         int
		validate     func(string) error
		want         string
		refused      bool
	}{
		{"local reserved", "127.0.0.1:16381", wireHost("127.0.0.1"), 6379, redisendpoint.Validate, "127.0.0.1:6379", true},
		{"remote substitution", "192.0.2.1:16381", wireHost("127.0.0.1"), 16380, redisendpoint.Validate, "192.0.2.1:16380", false},
		{"cached remote source", "192.0.2.1:6379", wireHost("127.0.0.1"), 6379, redisendpoint.Validate, "192.0.2.1:6379", false},
		{"dev reserved", "192.0.2.1:16381", wireHost("127.0.0.1"), 6379, redisendpoint.ValidateDev, "192.0.2.1:6379", true},
		{"empty", "127.0.0.1:16381", wireHost(""), 16380, redisendpoint.Validate, ":16380", true},
		{"null", "127.0.0.1:16381", nil, 16380, redisendpoint.Validate, ":16380", true},
		{"unspecified", "127.0.0.1:16381", wireHost("0.0.0.0"), 16380, redisendpoint.Validate, "0.0.0.0:16380", true},
		{"unknown", "127.0.0.1:16381", wireHost("?"), 16380, redisendpoint.Validate, "?:16380", true},
		{"tls zero port", "192.0.2.1:16381", wireHost("192.0.2.2"), 0, redisendpoint.Validate, "192.0.2.2:16381", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, wire := wireCluster(t, []string{tc.source}, func(string, int) string { return "*1\r\n" + slotWire(0, 16383, tc.host, tc.port) }, tc.validate)
			node, err := client.MasterForKey(context.Background(), "key")
			if err == nil {
				require.Equal(t, tc.want, node.Options().Addr)
				err = node.Ping(context.Background()).Err()
			}
			if tc.refused {
				require.Error(t, err)
				if tc.host != nil {
					var policy *RedisEndpointPolicyError
					require.ErrorAs(t, err, &policy)
				}
				wire.mu.Lock()
				attempts := wire.dials[tc.want]
				wire.mu.Unlock()
				require.Zero(t, attempts, "refused effective address must never reach the supplied dialer")
			} else {
				require.NoError(t, err)
				wire.mu.Lock()
				attempts := wire.dials[tc.want]
				local := wire.dials["127.0.0.1:"+strconv.Itoa(tc.port)]
				wire.mu.Unlock()
				require.Positive(t, attempts)
				if strings.HasPrefix(tc.source, "192.") {
					require.Zero(t, local)
				}
			}
		})
	}
}

func TestRedisClusterDiscoveryContinuesAfterMalformedSource(t *testing.T) {
	client, wire := wireCluster(t, []string{"127.0.0.1:16381", "127.0.0.2:16381"}, func(_ string, attempt int) string {
		if attempt == 1 {
			return "*1\r\n$9\r\nmalformed\r\n"
		}
		return "*1\r\n" + slotWire(0, 16383, wireHost("127.0.0.3"), 16380)
	}, redisendpoint.Validate)
	node, err := client.MasterForKey(context.Background(), "key")
	require.NoError(t, err)
	require.NoError(t, node.Ping(context.Background()).Err())
	wire.mu.Lock()
	defer wire.mu.Unlock()
	require.Len(t, wire.sources, 2)
	require.NotEqual(t, wire.sources[0], wire.sources[1])
}

func TestRedisClusterNodePolicyCoversSeedsAndRedirects(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	calls := 0
	options := RedisClusterOptions(&redis.UniversalOptions{Dialer: func(context.Context, string, string) (net.Conn, error) {
		calls++
		return nil, fmt.Errorf("unexpected dial")
	}})
	for _, addr := range []string{"127.0.0.1:6379", ":16380", "0.0.0.0:16380", "?:16380"} {
		node := options.NewClient(&redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, PipelinePoolSize: -1, Addr: addr, MaxRetries: -1, DialerRetries: 1, Dialer: options.Dialer})
		var policy *RedisEndpointPolicyError
		require.ErrorAs(t, node.Ping(context.Background()).Err(), &policy)
		require.NoError(t, node.Close())
	}
	require.Zero(t, calls)
}

func TestRedisClusterNativeLoaderOptions(t *testing.T) {
	options := RedisClusterOptions(&redis.UniversalOptions{})
	require.Nil(t, options.ClusterSlots, "native discovery must retain its real origin")
	require.True(t, options.DisableRoutingPolicies)
}

func TestRedisClusterRefusedNodeKeepsOtherSlots(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	slots := "*2\r\n" + slotWire(0, 8191, wireHost("127.0.0.1"), 16380) + slotWire(8192, 16383, wireHost("127.0.0.1"), 6379)
	client, wire := wireCluster(t, []string{"127.0.0.1:16381"}, func(string, int) string { return slots }, redisendpoint.Validate)
	allowed, err := client.MasterForKey(context.Background(), "b")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:16380", allowed.Options().Addr)
	require.NoError(t, allowed.Ping(context.Background()).Err())
	refused, err := client.MasterForKey(context.Background(), "a")
	require.NoError(t, err)
	var policy *RedisEndpointPolicyError
	require.ErrorAs(t, refused.Ping(context.Background()).Err(), &policy)
	wire.mu.Lock()
	defer wire.mu.Unlock()
	require.Zero(t, wire.dials["127.0.0.1:6379"])
}
