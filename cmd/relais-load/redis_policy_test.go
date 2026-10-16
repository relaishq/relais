package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedisToolsIgnoreLocal6379OptIn(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "1")
	for _, addr := range []string{"127.0.0.1:6379", "cache.example.invalid:6379"} {
		require.ErrorContains(t, pingRedis(context.Background(), addr, ""), "reserved")
	}
	_, cleanup, err := redisCommand(nil, "redis-server", "6379", t.TempDir(), "")
	defer cleanup()
	require.ErrorContains(t, err, "reserved")
}
