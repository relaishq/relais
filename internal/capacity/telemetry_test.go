package capacity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonitorUsesMediaWindowAndRetainsLabelledWireCounters(t *testing.T) {
	total := 0.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		total += 100
		_, _ = fmt.Fprintf(w, "# TYPE relais_transport_bytes_total counter\nrelais_transport_bytes_total{transport=\"redis\",direction=\"out\"} %g\n# TYPE relais_worker_active_sessions gauge\nrelais_worker_active_sessions 2\n# TYPE relais_process_cpu_seconds_total counter\nrelais_process_cpu_seconds_total 1\n# TYPE relais_process_rss_peak_bytes gauge\nrelais_process_rss_peak_bytes 1024\n", total)
	}))
	defer server.Close()
	monitor := Start(context.Background(), []Target{{Name: "worker-0", PID: os.Getpid(), URL: server.URL}}, time.Hour)
	time.Sleep(20 * time.Millisecond)
	report := monitor.Finish(2)
	require.Empty(t, report.Errors)
	require.Len(t, report.Processes, 1)
	p := report.Processes[0]
	require.Equal(t, 2, p.Samples)
	require.Positive(t, p.MeanRSSBytesPerCall)
	require.Equal(t, 2.0, p.Peaks["relais_worker_active_sessions"])
	require.InDelta(t, 100/(2*p.ObservedSeconds), p.RatesPerCallSecond[`relais_transport_bytes_total{direction="out",transport="redis"}`], .001)
}
func TestMissingServiceEvidenceCannotBeHidden(t *testing.T) {
	m := Start(context.Background(), []Target{{Name: "missing", PID: -10000}}, time.Hour)
	r := m.Finish(1)
	require.NotEmpty(t, r.Errors)
}
