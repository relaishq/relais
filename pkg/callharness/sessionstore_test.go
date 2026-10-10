package callharness_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/go-redis/redis/v8"
	"net"
	"os"
	"testing"
	"time"

	"github.com/relais/internal/redisendpoint"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// Every scenario uses the identical caller assertions with both store options.
// An absent or unavailable explicitly selected endpoint skips integration tests.
func forSessionStores(t *testing.T, scenario func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { scenario(t, nil) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("required Redis endpoint is not configured")
			}
			t.Skip("Redis harness requires RELAIS_TEST_REDIS_ADDR")
		}
		require.NoError(t, redisendpoint.Validate(addr), "tests must use a dedicated Redis")
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatalf("required Redis endpoint unavailable: %v", err)
			}
			t.Skipf("Redis harness endpoint unavailable: %v", err)
		}
		require.NoError(t, conn.Close())
		token := make([]byte, 16)
		_, err = rand.Read(token)
		require.NoError(t, err)
		prefix := "harness:" + hex.EncodeToString(token) + ":"
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, store.Close())
			client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
			defer func() { require.NoError(t, client.Close()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var cursor uint64
			for {
				keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
				require.NoError(t, err)
				for _, key := range keys {
					require.NoError(t, client.Del(ctx, key).Err())
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
		})
		scenario(t, store)
	})
}
