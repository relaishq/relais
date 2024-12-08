package benchmark

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// BenchmarkStorageWrite tests write performance of different storage backends.
// It measures how quickly each backend can store video frames.
func BenchmarkStorageWrite(b *testing.B) {
	ctx := context.Background()
	stores := map[string]storage.Storage{
		"memory": storage.NewMemoryStorage(),
	}

	// Try to connect to Redis if available
	if redisStore, err := storage.NewRedisStorage("localhost:6379"); err == nil {
		stores["redis"] = redisStore
		defer redisStore.Close()
	}

	// Generate test video frames
	generator := NewVideoGenerator(1280, 720, 30, time.Second)
	frames := generator.GenerateFrames()

	// Benchmark each storage backend
	for name, store := range stores {
		b.Run(name, func(b *testing.B) {
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
	stores := map[string]storage.Storage{
		"memory": storage.NewMemoryStorage(),
	}

	// Try to connect to Redis if available
	if redisStore, err := storage.NewRedisStorage("localhost:6379"); err == nil {
		stores["redis"] = redisStore
		defer redisStore.Close()
	}

	// Prepare test data
	generator := NewVideoGenerator(1280, 720, 30, time.Second)
	frames := generator.GenerateFrames()

	for name, store := range stores {
		// Pre-populate store with test data
		for _, frame := range frames {
			err := store.PutFrame(ctx, frame)
			require.NoError(b, err)
		}

		// Benchmark read operations
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, err := store.ListFrames(ctx, "test_session")
				require.NoError(b, err)
			}
		})
	}
}
