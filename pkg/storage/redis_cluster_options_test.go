package storage

import (
	"testing"

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
	commands, err := decodeClusterCommands([]interface{}{row})
	require.NoError(t, err)
	require.Equal(t, int8(1), commands["get"].FirstKeyPos)
	require.True(t, commands["get"].ReadOnly)
	for _, reply := range [][]interface{}{{"bad"}, {[]interface{}{int64(1)}}} {
		_, err := decodeClusterCommands(reply)
		require.Error(t, err)
	}
}
