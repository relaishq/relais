package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestProcessHandlerIsolatesRegistriesAndAllowsAdditionalCollectors(t *testing.T) {
	handler := ProcessHandler(func() []Sample {
		return []Sample{Counter("relais_test_packets_total", "Test packets.", 7), Gauge("relais_test_active", "Test active.", 2)}
	}, prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "relais_test_checkpoint_age_seconds", Help: "Extension point."}, func() float64 { return 0.1 }))
	for range 2 {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, r.Code)
		require.Contains(t, r.Header().Get("Content-Type"), "text/plain")
		require.Contains(t, r.Body.String(), "# TYPE relais_test_packets_total counter\nrelais_test_packets_total 7\n")
		require.Contains(t, r.Body.String(), "# TYPE relais_test_active gauge\nrelais_test_active 2\n")
		require.Contains(t, r.Body.String(), "relais_test_checkpoint_age_seconds 0.1\n")
		require.NotContains(t, r.Body.String(), "relais_egress_")
		require.NotContains(t, r.Body.String(), "relais_http_")
		require.NotContains(t, r.Body.String(), "go_")
	}
}
