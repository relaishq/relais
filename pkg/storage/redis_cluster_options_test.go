package storage

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestRedisClusterSlotMetadata(t *testing.T) {
	for _, extra := range []bool{false, true} {
		node := []interface{}{"127.0.0.1", int64(16380), "node-id"}
		if extra {
			node = append(node, []interface{}{"hostname", "cache.local"})
		}
		slots, err := decodeClusterSlots([]interface{}{[]interface{}{int64(0), int64(16383), node}}, "127.0.0.1:16380")
		require.NoError(t, err)
		require.Equal(t, []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{ID: "node-id", Addr: "127.0.0.1:16380"}}}}, slots)
	}
	for _, reply := range [][]interface{}{nil, {"bad"}, {[]interface{}{int64(-1), int64(16383), []interface{}{"host", int64(1)}}}, {[]interface{}{int64(0), int64(16384), []interface{}{"host", int64(1)}}}, {[]interface{}{int64(0), int64(16383), []interface{}{"host", int64(0)}}}} {
		_, err := decodeClusterSlots(reply, "127.0.0.1:16380")
		require.Error(t, err)
	}
	options := RedisClusterOptions(&redis.UniversalOptions{Addrs: []string{"127.0.0.1:16380"}, MaxRetries: -1, MaxRedirects: -1})
	require.False(t, options.ReadOnly)
	require.False(t, options.RouteByLatency)
	require.False(t, options.RouteRandomly)
	require.Equal(t, -1, options.MaxRetries)
	require.Equal(t, -1, options.MaxRedirects)
	require.NotNil(t, options.ClusterSlots)
}

func TestRedisClusterCommandMetadata(t *testing.T) {
	row := []interface{}{"get", int64(2), []interface{}{"readonly", "fast"}, int64(1), int64(1), int64(1), []interface{}{"@read"}, []interface{}{}, []interface{}{}, []interface{}{}}
	commands, skipped := decodeClusterCommands([]interface{}{row})
	require.Zero(t, skipped)
	require.Equal(t, int8(1), commands["get"].FirstKeyPos)
	require.True(t, commands["get"].ReadOnly)
	for _, reply := range [][]interface{}{{"bad"}, {[]interface{}{int64(1)}}} {
		commands, skipped := decodeClusterCommands(append(reply, row))
		require.Equal(t, len(reply), skipped)
		require.Contains(t, commands, "get")
	}
}

func TestRedisClusterDiscoveredAddressPolicy(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "0.0.0.0", "bad@host"} {
		_, err := decodeClusterSlots([]interface{}{[]interface{}{int64(0), int64(16383), []interface{}{host, int64(6379), "node"}}}, "192.0.2.1:16380")
		require.Error(t, err, host)
	}
	slots, err := decodeClusterSlots([]interface{}{[]interface{}{int64(0), int64(16383), []interface{}{"127.0.0.1", int64(16380), "node"}}}, "192.0.2.1:16380")
	require.NoError(t, err)
	require.Equal(t, "192.0.2.1:16380", slots[0].Nodes[0].Addr)
	_, err = decodeClusterSlots([]interface{}{[]interface{}{int64(0), int64(16383), []interface{}{"cache.example.invalid", int64(6379), "node"}, []interface{}{"127.0.0.1", int64(6379), "replica"}}}, "192.0.2.1:16380")
	require.Error(t, err, "a bad replica must reject the whole map")
}

func TestRedisClusterDiscoveryCandidatesAndTimeout(t *testing.T) {
	known := []string{"dead:16380", "healthy:16380", "other:16380"}
	got := discoveryCandidates(known, "healthy:16380")
	require.Equal(t, "healthy:16380", got[0])
	require.ElementsMatch(t, known, got)
	require.Equal(t, "dead:16380", known[0], "candidate order must not mutate shared state")
	opts := discoveryOptions(&redis.UniversalOptions{DialTimeout: 5 * time.Second, ReadTimeout: -1, WriteTimeout: time.Second}, "seed:16380")
	require.Equal(t, discoveryTimeout, opts.DialTimeout)
	require.Equal(t, discoveryTimeout, opts.ReadTimeout)
	require.Equal(t, discoveryTimeout, opts.WriteTimeout)
	require.Equal(t, 10*time.Millisecond, discoveryOptions(&redis.UniversalOptions{DialTimeout: 10 * time.Millisecond}, "seed:16380").DialTimeout)
}

func TestRedisClusterDiscoveryDeadline(t *testing.T) {
	budget := make(chan time.Duration, 1)
	options := RedisClusterOptions(&redis.UniversalOptions{Addrs: []string{"blackhole.example.invalid:16380"}, Dialer: func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("discovery dial has no deadline")
		}
		budget <- time.Until(deadline)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := options.ClusterSlots(ctx)
	require.Error(t, err)
	select {
	case value := <-budget:
		require.LessOrEqual(t, value, discoveryTimeout)
	default:
		t.Fatal("discovery did not use the supplied dialer")
	}
}
