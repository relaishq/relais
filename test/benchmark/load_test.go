package benchmark

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/relais/plugins/ingress/camera"
	"github.com/stretchr/testify/require"
)

// BenchmarkIngressThroughput measures the maximum frame ingestion rate.
// It runs a camera plugin and counts how many frames it can process per second.
func BenchmarkIngressThroughput(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := storage.NewMemoryStorage()
	plugin := camera.NewCameraPlugin()

	err := plugin.Initialize(ctx, map[string]interface{}{
		"fps": 30,
	})
	require.NoError(b, err)

	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		plugin.Run(ctx, store)
	}()

	time.Sleep(time.Second) // Let it run for 1 second
	cancel()
	wg.Wait()

	frames, err := store.ListFrames(ctx, "test_camera")
	require.NoError(b, err)

	b.ReportMetric(float64(len(frames)), "frames/sec")
}

// BenchmarkConcurrentClients tests system performance with multiple simultaneous clients.
// It measures how the system performs with different numbers of concurrent connections.
func BenchmarkConcurrentClients(b *testing.B) {
	clientCounts := []int{1, 10, 50, 100}

	for _, count := range clientCounts {
		b.Run(fmt.Sprintf("clients-%d", count), func(b *testing.B) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			store := storage.NewMemoryStorage()
			var wg sync.WaitGroup

			// Start camera plugin as source
			camera := camera.NewCameraPlugin()
			err := camera.Initialize(ctx, map[string]interface{}{
				"fps": 30,
			})
			require.NoError(b, err)

			wg.Add(1)
			go func() {
				defer wg.Done()
				camera.Run(ctx, store)
			}()

			// Start multiple egress clients
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(clientID int) {
					defer wg.Done()

					for {
						select {
						case <-ctx.Done():
							return
						default:
							_, err := store.ListFrames(ctx, "test_camera")
							if err != nil {
								b.Error(err)
								return
							}
							time.Sleep(time.Second / 30) // Simulate 30fps playback
						}
					}
				}(i)
			}

			wg.Wait()
		})
	}
}
