package benchmark

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// BenchmarkStorageWrite tests write performance of different storage backends.
// It measures how quickly each backend can store video frames.
func BenchmarkStorageWrite(b *testing.B) {
	ctx := context.Background()
	// Generate test video frames
	generator := NewVideoGenerator(1280, 720, 30, time.Second)
	frames := generator.GenerateFrames()

	// Benchmark each storage backend
	for _, name := range []string{"memory", "redis"} {
		b.Run(name, func(b *testing.B) {
			store := benchmarkStore(b, name)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, frame := range frames {
					err := store.PutFrame(ctx, frame)
					require.NoError(b, err)
				}
			}
		})
	}
}

// BenchmarkStorageRead tests read performance of different storage backends.
// It measures how quickly each backend can retrieve stored frames.
func BenchmarkStorageRead(b *testing.B) {
	ctx := context.Background()
	// Prepare test data
	generator := NewVideoGenerator(1280, 720, 30, time.Second)
	frames := generator.GenerateFrames()

	for _, name := range []string{"memory", "redis"} {
		b.Run(name, func(b *testing.B) {
			store := benchmarkStore(b, name)
			// Pre-populate store before timing read operations.
			for _, frame := range frames {
				err := store.PutFrame(ctx, frame)
				require.NoError(b, err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := store.ListFrames(ctx, "test_session")
				require.NoError(b, err)
			}
		})
	}
}

// benchmarkStore opens Redis only after the Redis subbenchmark is selected.
// Memory benchmarks still run when no Redis endpoint is configured.
func benchmarkStore(b *testing.B, name string) storage.Storage {
	b.Helper()
	if name == "memory" {
		return storage.NewMemoryStorage()
	}
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			b.Fatal("required Redis endpoint is not configured")
		}
		b.Skip("skipping: RELAIS_TEST_REDIS_ADDR not set")
	}
	token := make([]byte, 16)
	_, err := rand.Read(token)
	require.NoError(b, err)
	prefix := "benchmark:" + hex.EncodeToString(token) + ":"
	store, err := storage.NewRedisStorage(storage.RedisConfig{Addr: addr, Prefix: prefix})
	if err != nil {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			b.Fatalf("required Redis endpoint unavailable: %v", err)
		}
		b.Skipf("skipping: cannot connect to redis at %s: %v", addr, err)
	}
	b.Cleanup(func() {
		require.NoError(b, store.Close())
		client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
		defer func() { require.NoError(b, client.Close()) }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
			require.NoError(b, err)
			if len(keys) > 0 {
				require.NoError(b, client.Del(ctx, keys...).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	})
	return store
}
