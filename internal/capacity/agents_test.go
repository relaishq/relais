package capacity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAddWorkerDoesNotWaitForAgentStatusSweep(t *testing.T) {
	requested, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-requested:
		default:
			close(requested)
		}
		<-release
		_, _ = w.Write([]byte(`{"agent_session":{"id":"session","name":"custom/v1","checkpoint_known":true}}`))
	}))
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Monitor{targets: []Target{{Name: "worker-0", URL: server.URL, AgentSessions: []string{"session"}}}, retired: make([]atomic.Bool, 1), client: server.Client()}
	done := make(chan struct{})
	go func() { defer close(done); m.sampleAgents(ctx) }()
	defer func() { cancel(); unblock(); <-done }()
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("status request did not start")
	}
	added := make(chan error, 1)
	go func() { added <- m.AddWorker(Target{Name: "worker-1", URL: server.URL}) }()
	select {
	case err := <-added:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("AddWorker waited for a blocked status request")
	}
}

func TestAgentSamplerRetirementDuringRequest(t *testing.T) {
	requested, release, observed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requested)
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer old.Close()
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_session":{"id":"session","name":"custom/v1","checkpoint_known":true}}`))
		select {
		case observed <- struct{}{}:
		default:
		}
	}))
	defer next.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Monitor{targets: []Target{{Name: "worker-0", URL: old.URL, AgentSessions: []string{"session"}}, {Name: "worker-1", URL: next.URL}}, retired: make([]atomic.Bool, 2), client: old.Client()}
	done := make(chan struct{})
	go func() { defer close(done); m.sampleAgents(ctx) }()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	m.retired[0].Store(true)
	close(release)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("replacement not observed")
	}
	cancel()
	<-done
	require.Empty(t, m.data.Agents.Errors)
}

func TestWorkerIncarnationKeepsCountersAndAgeSampling(t *testing.T) {
	var count, statuses atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			statuses.Add(1)
			_, _ = w.Write([]byte(`{"agent_session":{"id":"session","name":"custom/v1","checkpoint_known":true}}`))
			return
		}
		value := count.Add(1)
		_, _ = fmt.Fprintf(w, "# TYPE relais_process_cpu_seconds_total counter\nrelais_process_cpu_seconds_total %d\n# TYPE relais_process_rss_peak_bytes gauge\nrelais_process_rss_peak_bytes 100\n# TYPE relais_events_total counter\nrelais_events_total %d\n", value, value)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := Target{Name: "worker-0", URL: server.URL, AgentSessions: []string{"session"}, PID: 1}
	m := Start(ctx, []Target{target}, 10*time.Millisecond)
	// Start accepts the first sample synchronously. Wait for another complete
	// sample before retiring; a handler hit alone can still be in flight.
	m.SampleNow(ctx)
	// Agent sweeps are serial: a second request proves the preceding status
	// was accepted, without racing an unsynchronised read of monitor data.
	require.Eventually(t, func() bool { return statuses.Load() >= 2 }, 3*time.Second, 10*time.Millisecond)
	m.Retire("worker-0")
	target.PID = 2
	require.NoError(t, m.AddWorker(target))
	m.SampleNow(ctx)
	m.SampleNow(ctx)
	before := statuses.Load()
	require.Eventually(t, func() bool { return statuses.Load() >= before+2 }, 3*time.Second, 10*time.Millisecond)
	result := m.Finish(1)
	require.Len(t, result.Processes, 2)
	require.Equal(t, 1, result.Processes[0].PID)
	require.Equal(t, 2, result.Processes[1].PID)
	require.False(t, result.Processes[0].UnexpectedExit)
	for _, process := range result.Processes {
		require.GreaterOrEqual(t, process.Samples, 2)
		require.Positive(t, process.ObservedSeconds)
		require.Positive(t, process.CPUSeconds)
		require.Positive(t, process.RatesPerCallSecond["relais_events_total"])
	}
	require.Empty(t, result.Errors)
	require.Empty(t, result.Agents.Errors)
	require.NotEmpty(t, result.Agents.Samples)
	require.Error(t, m.AddWorker(Target{Name: "store"}))
}

func TestPlannedDrainCapturesFinalExportBeforeRetirement(t *testing.T) {
	var peak atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "# TYPE relais_process_cpu_seconds_total counter\nrelais_process_cpu_seconds_total 1\n# TYPE relais_process_rss_peak_bytes gauge\nrelais_process_rss_peak_bytes 100\n# TYPE relais_worker_export_http_peak_bytes gauge\nrelais_worker_export_http_peak_bytes %d\n", peak.Load())
	}))
	defer server.Close()
	m := &Monitor{targets: []Target{{Name: "worker-0", URL: server.URL}}, stores: []*redis.Client{nil}, retired: make([]atomic.Bool, 1), client: server.Client(), data: Telemetry{Processes: []Process{{Name: "worker-0", Peaks: map[string]float64{}}}}}
	m.SampleNow(context.Background())
	peak.Store(4096) // Drain completed since the last periodic observation.
	m.SampleNow(context.Background())
	m.Retire("worker-0")
	require.EqualValues(t, 4096, m.data.Processes[0].Peaks["relais_worker_export_http_peak_bytes"])
	require.Equal(t, 2, m.data.Processes[0].Samples)
	require.True(t, m.retired[0].Load())
}
