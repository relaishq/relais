package capacity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
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
	require.InDelta(t, 100/(2*report.WindowSeconds), p.RatesPerCallSecond[`relais_transport_bytes_total{direction="out",transport="redis"}`], .001)
}
func TestMissingServiceEvidenceCannotBeHidden(t *testing.T) {
	m := Start(context.Background(), []Target{{Name: "missing", PID: -10000}}, time.Hour)
	r := m.Finish(1)
	require.NotEmpty(t, r.Errors)
}

func TestMonitorReportsUnretiredExitAndChildError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "# TYPE relais_process_cpu_seconds_total counter\nrelais_process_cpu_seconds_total 1\n# TYPE relais_process_rss_peak_bytes gauge\nrelais_process_rss_peak_bytes 1024\n")
	}))
	defer server.Close()
	done := make(chan struct{})
	m := Start(context.Background(), []Target{{Name: "worker-0", URL: server.URL, Done: done, Err: func() error { return errors.New("exit status 2: panic") }}}, time.Hour)
	m.sample(context.Background())
	close(done)
	r := m.Finish(2)
	require.NotEmpty(t, r.Errors, "a crashed worker with valid pre-crash samples cannot pass")
	require.Contains(t, fmt.Sprint(r.Errors), "exit status 2: panic")
}

func TestPartialProcessUsesSharedStepDenominator(t *testing.T) {
	p := Process{Name: "killed", Samples: 2, ObservedSeconds: 5, CPUSeconds: 10, RSSByteSeconds: 1000, HeapByteSeconds: 500, First: map[string]float64{"relais_store_commands_total": 0}, Last: map[string]float64{"relais_store_commands_total": 600}}
	require.Empty(t, normalizeProcess(&p, 2, 30))
	require.InDelta(t, 10.0/60, p.CPUSecondsPerCallSecond, 1e-9)
	require.Equal(t, 10.0, p.RatesPerCallSecond["relais_store_commands_total"])
	require.Equal(t, 100.0, p.MeanRSSBytesPerCall)
	require.Equal(t, 50.0, p.MeanHeapBytesPerCall)
}

func TestMonitorIntentionalRetirementDoesNotHidePriorExit(t *testing.T) {
	done := make(chan struct{})
	m := Start(context.Background(), []Target{{Name: "worker-0", Done: done}}, time.Hour)
	close(done)
	m.Retire("worker-0")
	require.True(t, m.Finish(1).Processes[0].UnexpectedExit)
}

func TestMonitorPreservesRealChildExitError(t *testing.T) {
	if marker := os.Getenv("RELAIS_CAPACITY_EXIT_MARKER"); marker != "" {
		for {
			if _, err := os.Stat(marker); err == nil {
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	marker := dir + "/exit"
	child, err := clusterprocess.Start(dir, "unplanned-exit", append(os.Environ(), "RELAIS_CAPACITY_EXIT_MARKER="+marker), executable, "-test.run=^TestMonitorPreservesRealChildExitError$")
	require.NoError(t, err)
	defer child.Stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "# TYPE relais_process_cpu_seconds_total counter\nrelais_process_cpu_seconds_total 1\n# TYPE relais_process_rss_peak_bytes gauge\nrelais_process_rss_peak_bytes 1024\n")
	}))
	defer server.Close()
	monitor := Start(context.Background(), []Target{{Name: "worker-0", URL: server.URL, PID: child.PID(), Done: child.Done(), Err: child.Err}}, time.Hour)
	monitor.sample(context.Background())
	require.NoError(t, os.WriteFile(marker, nil, 0600))
	select {
	case <-child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("owned child did not exit")
	}
	report := monitor.Finish(2)
	require.True(t, report.Processes[0].UnexpectedExit)
	require.Error(t, child.Err())
	require.Contains(t, fmt.Sprint(report.Errors), child.Err().Error())
	require.GreaterOrEqual(t, report.Processes[0].Samples, 2)
}
