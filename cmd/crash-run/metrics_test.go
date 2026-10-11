package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/metrics"
	"github.com/stretchr/testify/require"
)

func TestScrapeMetricsPreservesLabelsCountersAndHistogramTotals(t *testing.T) {
	body := `# TYPE relais_worker_packets_in_total counter
relais_worker_packets_in_total 12
# TYPE relais_worker_active_sessions gauge
relais_worker_active_sessions 1
# TYPE relais_checkpoint_writes_total counter
relais_checkpoint_writes_total{outcome="ok"} 4
relais_checkpoint_writes_total{outcome="error"} 0
# TYPE relais_checkpoint_write_seconds histogram
relais_checkpoint_write_seconds_bucket{le="+Inf"} 4
relais_checkpoint_write_seconds_sum 0.25
relais_checkpoint_write_seconds_count 4
# TYPE go_goroutines gauge
go_goroutines 8
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/metrics", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		require.Contains(t, r.Header.Get("Accept"), "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	values, counters, err := scrapeMetrics(context.Background(), server.Client(), server.URL+"/metrics")
	require.NoError(t, err)
	require.Len(t, values, 6)
	require.EqualValues(t, 12, values["relais_worker_packets_in_total"])
	require.True(t, counters["relais_worker_packets_in_total"])
	require.False(t, counters["relais_worker_active_sessions"])
	require.EqualValues(t, 4, values[`relais_checkpoint_writes_total{outcome="ok"}`])
	require.EqualValues(t, 0, values[`relais_checkpoint_writes_total{outcome="error"}`])
	require.EqualValues(t, 4, values["relais_checkpoint_write_seconds_count"])
	require.Equal(t, 0.25, values["relais_checkpoint_write_seconds_sum"])
}

func TestScrapeMetricsRejectsUnavailableOrInvalidEvidence(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"status", "error", http.StatusServiceUnavailable},
		{"empty", "# no samples\n", http.StatusOK},
		{"broken", "relais_broken{\n", http.StatusOK},
		{"oversized", strings.Repeat("x", (1<<20)+1), http.StatusOK},
		{"nan", "# TYPE relais_test gauge\nrelais_test NaN\n", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			values, _, err := scrapeMetrics(context.Background(), server.Client(), server.URL+"/metrics")
			require.Error(t, err)
			require.Nil(t, values)
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := scrapeMetrics(ctx, server.Client(), server.URL+"/metrics")
	require.Error(t, err)
	server.Close()
	_, _, err = scrapeMetrics(context.Background(), server.Client(), server.URL+"/metrics")
	require.Error(t, err)
}

func TestMetricsRunRetainsExitedProcessAndSeparatesReplacement(t *testing.T) {
	oldInterval := metricsInterval
	metricsInterval = 10 * time.Millisecond
	defer func() { metricsInterval = oldInterval }()
	var packets atomic.Uint64
	packets.Store(10)
	server := httptest.NewServer(metrics.ProcessHandler(func() []metrics.Sample {
		return []metrics.Sample{metrics.Counter("relais_relay_caller_packets_total", "Packets.", packets.Load()), metrics.Gauge("relais_relay_active_routes", "Routes.", 1)}
	}))
	defer server.Close()
	r := newMetricsRun()
	defer func() { r.cancel(); <-r.done }()
	done := make(chan struct{})
	old := &metricsTarget{url: server.URL + "/metrics", done: done, data: ProcessMetrics{Process: "relay"}}
	r.mu.Lock()
	r.targets = append(r.targets, old)
	r.mu.Unlock()
	r.sample(context.Background())
	packets.Store(20)
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return old.data.Samples >= 2 && old.data.Values["relais_relay_caller_packets_total"] == 20
	}, time.Second, time.Millisecond)
	close(done)
	packets.Store(0) // replacement starts at zero on the very same URL
	r.mu.Lock()
	r.targets = append(r.targets, &metricsTarget{url: server.URL + "/metrics", done: make(chan struct{}), data: ProcessMetrics{Process: "relay-restarted"}})
	r.mu.Unlock()
	dir := t.TempDir()
	out := r.finish(dir)
	require.Len(t, out, 2)
	require.True(t, out[0].Exited)
	require.EqualValues(t, 20, out[0].Values["relais_relay_caller_packets_total"])
	require.Positive(t, out[0].Rates["relais_relay_caller_packets_total"])
	require.EqualValues(t, 1, out[0].GaugePeaks["relais_relay_active_routes"])
	require.False(t, out[1].Exited)
	require.Zero(t, out[1].Values["relais_relay_caller_packets_total"])
	encoded, err := os.ReadFile(filepath.Join(dir, "metrics.json"))
	require.NoError(t, err)
	var decoded []ProcessMetrics
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Len(t, decoded, len(out))
	for i := range out {
		require.Equal(t, out[i].Process, decoded[i].Process)
		require.Equal(t, out[i].Samples, decoded[i].Samples)
		require.Equal(t, out[i].Values, decoded[i].Values)
		require.Equal(t, out[i].Exited, decoded[i].Exited)
		require.True(t, out[i].LastAt.Equal(decoded[i].LastAt))
	}
	require.Equal(t, out[0].Rates, decoded[0].Rates)
}

func TestMetricsRunKeepsMissingScrapeExplicit(t *testing.T) {
	oldInterval := metricsInterval
	metricsInterval = 0
	defer func() { metricsInterval = oldInterval }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	r := newMetricsRun()
	defer func() { r.cancel(); <-r.done }()
	r.mu.Lock()
	r.targets = append(r.targets, &metricsTarget{url: server.URL + "/metrics", done: make(chan struct{}), data: ProcessMetrics{Process: "worker-0"}})
	r.mu.Unlock()
	out := r.finish(t.TempDir())
	require.Zero(t, out[0].Samples)
	require.Nil(t, out[0].Values, "failed scrapes must not fabricate zero counters")
	require.Contains(t, out[0].Error, "503")
}

func manualMetricsRun(t *testing.T) *metricsRun {
	t.Helper()
	oldInterval := metricsInterval
	metricsInterval = 0
	t.Cleanup(func() { metricsInterval = oldInterval })
	r := newMetricsRun()
	t.Cleanup(func() { r.cancel(); <-r.done })
	return r
}

func TestPreFaultScrapeBudgetDoesNotWaitForNetworkLock(t *testing.T) {
	r := manualMetricsRun(t)
	entered, release := make(chan struct{}), make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = w.Write([]byte("relais_test 1\n"))
		case <-req.Context().Done():
		}
	}))
	defer busy.Close()
	var slowCalls atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		slowCalls.Add(1)
		<-req.Context().Done()
	}))
	defer slow.Close()
	r.targets = []*metricsTarget{
		{url: busy.URL, done: make(chan struct{})},
		{url: slow.URL, done: make(chan struct{})},
		{url: slow.URL, done: make(chan struct{})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); r.sample(ctx) }()
	defer func() { cancel(); close(release); <-finished }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("periodic scrape did not start")
	}
	locked := r.mu.TryLock()
	if locked {
		r.mu.Unlock()
	}
	require.True(t, locked, "network I/O must not hold the state lock")
	started := time.Now()
	r.sampleBeforeFault(context.Background())
	require.Less(t, time.Since(started), 450*time.Millisecond, "one 200 ms budget covers all targets")
	require.EqualValues(t, 1, slowCalls.Load(), "deadline prevents scraping remaining targets")
}

func TestMetricsRunClearsExitedScrapeErrors(t *testing.T) {
	r := manualMetricsRun(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	done := make(chan struct{})
	target := &metricsTarget{url: server.URL, done: done}
	r.targets = []*metricsTarget{target}
	r.sample(context.Background())
	require.Contains(t, target.data.Error, "503")
	close(done)
	r.sample(context.Background())
	require.True(t, target.data.Exited)
	require.Empty(t, target.data.Error)
	require.Zero(t, target.data.Samples, "exit must not manufacture successful observations")
}

func TestMetricsRunDiscardsResponseAfterProcessExit(t *testing.T) {
	r := manualMetricsRun(t)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("relais_test 999\n"))
	}))
	defer server.Close()
	done := make(chan struct{})
	target := &metricsTarget{url: server.URL, done: done}
	r.targets = []*metricsTarget{target}
	finished := make(chan struct{})
	go func() { defer close(finished); r.sample(context.Background()) }()
	released := false
	defer func() {
		if !released {
			close(release)
		}
		<-finished
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scrape did not start")
	}
	close(done)
	r.sample(context.Background())
	close(release)
	released = true
	<-finished
	r.mu.Lock()
	exited, scrapeError, samples := target.data.Exited, target.data.Error, target.data.Samples
	r.mu.Unlock()
	require.True(t, exited)
	require.Empty(t, scrapeError)
	require.Zero(t, samples)
}
