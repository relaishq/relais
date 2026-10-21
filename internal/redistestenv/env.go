// Package redistestenv reads test endpoints without contacting Redis.
package redistestenv

import (
	"os"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/redisendpoint"
)

// Address leaves missing-endpoint skip/require decisions to each test.
// A configured endpoint must pass the strict development policy before I/O.
// Port 6379 is refused on every host, regardless of the local opt-in.
func Address(t testing.TB) string {
	t.Helper()
	return read(t, "RELAIS_TEST_REDIS_ADDR")
}

// ClusterAddresses validates every configured seed before any caller can use it.
// The CSV is preserved so callers retain their existing parsing and skip rules.
func ClusterAddresses(t testing.TB) string {
	t.Helper()
	return read(t, "RELAIS_TEST_REDIS_CLUSTER_ADDRS")
}

func read(t testing.TB, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value != "" {
		addrs := []string{value}
		if name == "RELAIS_TEST_REDIS_CLUSTER_ADDRS" {
			addrs = strings.Split(value, ",")
			for i := range addrs {
				addrs[i] = strings.TrimSpace(addrs[i])
			}
		}
		for _, addr := range addrs {
			if err := redisendpoint.ValidateDev(addr); err != nil {
				t.Fatalf("%s: %v; configure a dedicated test Redis", name, err)
			}
		}
	}
	return value
}
