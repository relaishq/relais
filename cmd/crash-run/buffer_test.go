package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBufferMetricsRequireSurvivingWorkerDecryptEvidence(t *testing.T) {
	metrics := []ProcessMetrics{
		{Process: "worker-0", Exited: true},
		{Process: "worker-1", Samples: 1, Values: map[string]float64{"relais_worker_decryption_failures_total": 0}},
		{Process: "relay", Samples: 1, Values: map[string]float64{}},
	}
	for _, name := range []string{"buffer_sessions", "buffer_packets", "buffer_bytes", "buffer_drops_total", "buffer_expired_packets_total", "buffer_untracked_total", "replays_total", "replay_packets_total", "replay_filtered_packets_total", "replay_drops_total", "replay_send_failures_total", "replay_expiries_total"} {
		metrics[2].Values["relais_relay_"+name] = 0
	}
	require.NoError(t, validateBufferMetrics(result{Metrics: metrics}, true, false))
	metrics[1].Values["relais_worker_replay_rejections_total"] = 1
	require.ErrorContains(t, validateBufferMetrics(result{Metrics: metrics}, true, false), "recorded replay rejections")
	metrics[1].Values["relais_worker_replay_rejections_total"] = 0
	metrics[1].Values["relais_worker_decryption_failures_total"] = 1
	require.ErrorContains(t, validateBufferMetrics(result{Metrics: metrics}, true, false), "recorded decryption failures")
	delete(metrics[1].Values, "relais_worker_decryption_failures_total")
	require.ErrorContains(t, validateBufferMetrics(result{Metrics: metrics}, true, false), "metrics unavailable")
}
