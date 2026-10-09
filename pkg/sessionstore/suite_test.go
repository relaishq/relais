package sessionstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func forStores(t *testing.T, test func(*testing.T, func(*testing.T) Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, func(*testing.T) Store { return NewMemory() }) })
	t.Run("redis", func(t *testing.T) { test(t, func(t *testing.T) Store { return testRedis(t) }) })
}
func testRedis(t *testing.T, options ...RedisOptions) *Redis {
	t.Helper()
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("required Redis endpoint is not configured")
		}
		t.Skip("Redis integration requires RELAIS_TEST_REDIS_ADDR")
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatalf("required Redis endpoint unavailable: %v", err)
		}
		t.Skipf("Redis test endpoint unavailable: %v", err)
	}
	require.NoError(t, conn.Close())
	token := make([]byte, 16)
	_, err = rand.Read(token)
	require.NoError(t, err)
	r, err := NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "session-test:" + hex.EncodeToString(token) + ":"}, make([]byte, 32), options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		r.maintenance.Wait()
		ctx := context.Background()
		var cursor uint64
		for {
			keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 100).Result()
			require.NoError(t, err)
			for _, key := range keys {
				require.NoError(t, r.client.Del(ctx, key).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		require.NoError(t, r.Close())
	})
	return r
}
