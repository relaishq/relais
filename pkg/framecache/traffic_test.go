package framecache

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/relais/pkg/metrics"
	"github.com/stretchr/testify/require"
)

func frameTraffic(t *testing.T) (float64, float64) {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.ProcessHandler(func() []metrics.Sample { return nil }).ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(response.Body.String()))
	require.NoError(t, err)
	commands, bytes := 0.0, 0.0
	for _, m := range families["relais_store_commands_total"].Metric {
		for _, label := range m.Label {
			if label.GetName() == "store" && label.GetValue() == "frame" {
				commands += m.GetCounter().GetValue()
			}
		}
	}
	for _, m := range families["relais_transport_bytes_total"].Metric {
		labels := map[string]string{}
		for _, label := range m.Label {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["client"] == "frame" && labels["transport"] == "redis" {
			bytes += m.GetCounter().GetValue()
		}
	}
	return commands, bytes
}

func TestRedisTrafficHasFrameClientLabel(t *testing.T) {
	store := testRedis(t, Limits{})
	commands, bytes := frameTraffic(t)
	require.NoError(t, store.Append(context.Background(), "frame-traffic", testFrame(1, true)))
	afterCommands, afterBytes := frameTraffic(t)
	require.Greater(t, afterCommands, commands)
	require.Greater(t, afterBytes, bytes)
}
