package sessionstore

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/relais/internal/redisendpoint"
	"github.com/relais/pkg/storage"
)

// Setup uses the real epoch-fenced writer; only the eager restore is timed.
func BenchmarkRedisRouteRestore(b *testing.B) {
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		b.Skip("dedicated Redis not configured")
	}
	if err := redisendpoint.Validate(addr); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := context.Background()
			store, err := NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: "route-bench:" + rand.Text() + ":"}, make([]byte, 32))
			if err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
			}()
			caller := netip.MustParseAddr("::1")
			owner := netip.MustParseAddrPort("127.0.0.1:4999")
			ids := make([]string, count)
			for i := range count {
				ids[i] = fmt.Sprintf("session-%d", i)
				lease, err := store.Claim(ctx, ids[i], owner, 10*time.Minute)
				if err != nil {
					b.Fatal(err)
				}
				now := time.Now().UTC()
				route := Route{Caller: netip.AddrPortFrom(caller, uint16(10000+i)), SessionID: ids[i], Generation: lease.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(5 * time.Minute)}
				if err := store.PutRoute(ctx, route, false); err != nil {
					b.Fatal(err)
				}
			}
			defer func() {
				for _, id := range ids {
					if err := store.ForgetRoutes(ctx, id); err != nil {
						b.Fatal(err)
					}
				}
			}()
			b.ResetTimer()
			start := time.Now()
			for range b.N {
				routes, owners, err := store.LoadRouteOwners(ctx, count)
				if err != nil || len(routes) != count || len(owners) != count {
					b.Fatalf("restore routes=%d leases=%d error=%v", len(routes), len(owners), err)
				}
			}
			elapsed := time.Since(start)
			b.StopTimer()
			b.ReportMetric(float64(count*b.N)/elapsed.Seconds(), "routes/s")
		})
	}
}
