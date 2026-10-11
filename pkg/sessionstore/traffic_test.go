package sessionstore

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/common/expfmt"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func storeTraffic(t *testing.T) (float64, float64) {
	t.Helper()
	r := httptest.NewRecorder()
	metrics.ProcessHandler(func() []metrics.Sample { return nil }).ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(r.Body.String()))
	require.NoError(t, err)
	var commands, bytes float64
	for _, m := range families["relais_store_commands_total"].Metric {
		for _, label := range m.Label {
			if label.GetName() == "store" && label.GetValue() == "session" {
				commands += m.GetCounter().GetValue()
			}
		}
	}
	for _, m := range families["relais_transport_bytes_total"].Metric {
		for _, label := range m.Label {
			if label.GetName() == "transport" && label.GetValue() == "redis" {
				bytes += m.GetCounter().GetValue()
			}
		}
	}
	return commands, bytes
}
func TestRedisTrafficIncludesEncryptedCheckpointAndOwnersClient(t *testing.T) {
	store := testRedis(t)
	ctx := context.Background()
	lease, err := store.Claim(ctx, "traffic-checkpoint", netip.MustParseAddrPort("127.0.0.1:10101"), time.Minute)
	require.NoError(t, err)
	beforeCommands, beforeBytes := storeTraffic(t)
	state := []byte(strings.Repeat("synthetic checkpoint ", 100))
	require.NoError(t, store.PutState(ctx, lease, state))
	afterCommands, afterBytes := storeTraffic(t)
	require.Greater(t, afterCommands, beforeCommands)
	require.Greater(t, afterBytes-beforeBytes, float64(len(state)))
	owners, err := NewRedisOwners(ctx, storage.RedisConfig{Addr: store.client.(*redis.Client).Options().Addr, Prefix: store.prefix})
	require.NoError(t, err)
	defer func() { _ = owners.Close() }()
	_, err = owners.Owner(ctx, lease.SessionID)
	require.NoError(t, err)
	commands, _ := storeTraffic(t)
	require.GreaterOrEqual(t, commands-afterCommands, 2.0)
}
