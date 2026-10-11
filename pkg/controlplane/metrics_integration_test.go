package controlplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func readMetrics(t *testing.T, handler http.Handler) map[string]float64 {
	t.Helper()
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, r.Code)
	require.Contains(t, r.Header().Get("Content-Type"), "text/plain")
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(r.Body)
	require.NoError(t, err)
	values := make(map[string]float64)
	for name, family := range families {
		for _, metric := range family.Metric {
			key := name
			for _, label := range metric.Label {
				require.True(t, strings.HasPrefix(name, "relais_checkpoint_"))
				require.Contains(t, []string{"result", "policy"}, label.GetName(), "no caller or session labels")
				key += "{" + label.GetName() + "=" + strconv.Quote(label.GetValue()) + "}"
			}
			switch {
			case metric.Counter != nil:
				values[key] = metric.Counter.GetValue()
			case metric.Histogram != nil:
				values[key+"_count"] = float64(metric.Histogram.GetSampleCount())
				values[key+"_sum"] = metric.Histogram.GetSampleSum()
			default:
				values[key] = metric.Gauge.GetValue()
			}
		}
	}
	require.NotContains(t, values, "relais_egress_empty_polls_total")
	require.NotContains(t, values, "relais_http_requests_in_flight")
	return values
}

// A real caller moves from worker 0 to 1, then survives worker 1's hard kill.
// Assertions read the same private metrics handlers used by the binaries.
func TestMetricsAcrossHTTPMoveAndTakeover(t *testing.T) {
	disableProbe := workerprobe.Enable()
	defer disableProbe()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store, Routes: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	plane := controlplane.New(r, store)
	var workers []*mediaworker.Worker
	for _, name := range []string{"0", "1"} {
		worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
		require.NoError(t, err)
		defer func() { require.NoError(t, worker.Close()) }()
		r.AddWorker(worker.LocalAddr())
		require.NoError(t, plane.Register(name, worker.LocalAddr(), worker))
		workers = append(workers, worker)
	}
	done := make(chan error, 1)
	go func() { done <- plane.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	server := httptest.NewServer(plane.ProcessHandler())
	defer server.Close()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	call, err := h.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 250*time.Millisecond))
	relayBefore := readMetrics(t, r.PrivateHandler())
	require.EqualValues(t, 1, relayBefore["relais_relay_active_routes"])
	require.Positive(t, relayBefore["relais_relay_caller_packets_total"])
	require.Positive(t, relayBefore["relais_relay_worker_packets_total"])
	require.Eventually(t, func() bool { return r.Stats().RouteWrites > 0 }, time.Second, time.Millisecond)
	_, err = plane.Move(ctx, call.SessionID(), "1")
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 250*time.Millisecond))
	moved := readMetrics(t, workers[1].PrivateHandler())
	require.EqualValues(t, 1, moved["relais_worker_active_sessions"])
	require.EqualValues(t, 1, moved["relais_worker_handovers_resumed_total"])
	require.Zero(t, moved["relais_worker_takeovers_resumed_total"])
	require.Zero(t, readMetrics(t, workers[0].PrivateHandler())["relais_worker_active_sessions"])
	beforeTakeover := readMetrics(t, plane.ProcessHandler())
	beforeWrites := moved[`relais_checkpoint_writes_total{result="success"}`]
	require.NoError(t, workerprobe.Kill(workers[1].LocalAddr()))
	require.Eventually(t, func() bool {
		status, err := plane.Status(ctx)
		return err == nil && len(status.Calls) == 1 && status.Calls[0].Owner == "0" && len(status.Takeovers) == 1 && !status.Takeovers[0].Lost
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, call.SendMedia(ctx, 250*time.Millisecond))
	resumed := readMetrics(t, workers[0].PrivateHandler())
	require.EqualValues(t, 1, resumed["relais_worker_takeovers_resumed_total"])
	require.Zero(t, resumed["relais_worker_handovers_resumed_total"])
	require.Positive(t, resumed["relais_worker_packets_in_total"])
	require.Positive(t, resumed["relais_worker_packets_out_total"])
	require.Zero(t, resumed["relais_worker_decryption_failures_total"])
	control := readMetrics(t, plane.ProcessHandler())
	for _, name := range []string{"calls", "moves", "takeovers", "detection_events"} {
		require.EqualValues(t, 1, control["relais_control_"+name+"_total"], name)
	}
	require.EqualValues(t, 1, control["relais_control_active_calls"])
	require.Zero(t, control["relais_control_losses_total"])
	require.Equal(t, beforeTakeover["relais_checkpoint_age_seconds_count"]+1, control["relais_checkpoint_age_seconds_count"])
	require.Greater(t, control["relais_checkpoint_age_seconds_sum"], beforeTakeover["relais_checkpoint_age_seconds_sum"])
	require.Greater(t, resumed[`relais_checkpoint_writes_total{result="success"}`], beforeWrites)
	relayAfter := readMetrics(t, r.PrivateHandler())
	require.Greater(t, relayAfter["relais_relay_caller_packets_total"], relayBefore["relais_relay_caller_packets_total"])
	require.Greater(t, relayAfter["relais_relay_worker_packets_total"], relayBefore["relais_relay_worker_packets_total"])
	require.Zero(t, relayAfter["relais_relay_held_packets"])
	require.Zero(t, relayAfter["relais_relay_hold_expiries_total"])
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, readMetrics(t, workers[0].PrivateHandler())["relais_worker_active_sessions"])
	ended := readMetrics(t, plane.ProcessHandler())
	require.Zero(t, ended["relais_control_active_calls"])
	require.EqualValues(t, 1, ended["relais_control_moves_total"], "lifetime metrics survive call removal")
	require.EqualValues(t, 1, ended["relais_control_takeovers_total"])
}
