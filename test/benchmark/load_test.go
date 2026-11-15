package benchmark

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// BenchmarkConcurrentClients measures frame-storage readers alongside a writer.
// Each iteration writes one frame and reads once per client; setup is not timed.
func BenchmarkConcurrentClients(b *testing.B) {
	for _, count := range []int{1, 10, 50, 100} {
		b.Run(fmt.Sprintf("clients-%d", count), func(b *testing.B) {
			ctx := context.Background()
			store := storage.NewMemoryStorage()
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			frames := NewVideoGenerator(64, 48, 30, time.Second).GenerateFrames()
			for _, frame := range frames {
				require.NoError(b, store.PutFrame(ctx, frame))
			}

			var wg sync.WaitGroup
			b.ResetTimer()
			wg.Go(func() {
				for j := 0; j < b.N; j++ {
					if err := store.PutFrame(ctx, frames[j%len(frames)]); err != nil {
						b.Error(err)
						return
					}
				}
			})
			for i := 0; i < count; i++ {
				wg.Go(func() {
					for j := 0; j < b.N; j++ {
						if _, err := store.ListFrames(ctx, "test_session"); err != nil {
							b.Error(err)
							return
						}
					}
				})
			}
			wg.Wait()
		})
	}
}
