package main

import (
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func customFixture() capacityStep {
	r := capacityFixture()
	r.Duration = time.Second
	r.Telemetry.WindowSeconds = 1
	for i := 0; i < 20; i++ {
		r.Telemetry.Agents.Samples = append(r.Telemetry.Agents.Samples, capacity.AgentSample{Status: mediaworker.AgentSessionStatus{CheckpointAgeMS: 100, SnapshotAgeMS: 120, PendingBytes: 1024}})
	}
	first, last := map[string]float64{}, map[string]float64{}
	for _, name := range []string{"events", "save_calls", "save_wall_nanoseconds", "save_cpu_nanoseconds", "save_cpu_unavailable", "forced_checkpoint_attempts", "forced_checkpoint_successes", "forced_checkpoint_failures", "snapshot_debit_bytes"} {
		first["relais_worker_agent_"+name+"_total"] = 0
		last["relais_worker_agent_"+name+"_total"] = 0
	}
	last["relais_worker_agent_events_total"] = 100
	last["relais_worker_agent_save_calls_total"] = 101
	last["relais_worker_agent_save_wall_nanoseconds_total"] = 100000
	last["relais_worker_agent_save_cpu_nanoseconds_total"] = 50000
	r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "worker-0", First: first, Last: last, Peaks: map[string]float64{"relais_worker_export_http_peak_bytes": 4096}})
	r.Processes[0].Calls = append(r.Processes[0].Calls, r.Processes[0].Calls[0])
	for i := range r.Processes[0].Calls {
		report := *r.Processes[0].Calls[i].Call
		report.Tracks = []callharness.TrackReport{{Kind: "audio", Packets: 1}}
		r.Processes[0].Calls[i].Call = &report
	}
	return capacityStep{Run: r}
}
func TestCustomCapacityBlocksIncompleteAndOverBudgetEvidence(t *testing.T) {
	baseline := customFixture()
	e := customEvidence(baseline.Run)
	require.Equal(t, "pass", e.Verdict, e.Blocking)
	require.Equal(t, 1e-6, e.SaveWallSecondsPerEvent)
	require.Equal(t, 5e-7, e.SaveCPUSecondsPerEvent)
	for _, change := range []string{"checkpoint", "snapshot", "missing-cpu", "unsupported-cpu", "export", "samples", "audio"} {
		t.Run(change, func(t *testing.T) {
			r := customFixture().Run
			switch change {
			case "checkpoint":
				r.Telemetry.Agents.Samples[0].Status.CheckpointAgeMS = 551
			case "snapshot":
				r.Telemetry.Agents.Samples[0].Status.SnapshotAgeMS = 551
			case "missing-cpu":
				delete(r.Telemetry.Processes[2].Last, "relais_worker_agent_save_cpu_nanoseconds_total")
			case "unsupported-cpu":
				r.Telemetry.Processes[2].Last["relais_worker_agent_save_cpu_unavailable_total"] = 101
			case "export":
				r.Telemetry.Processes[2].Peaks = nil
			case "samples":
				r.Telemetry.Agents.Samples = nil
			case "audio":
				r.Processes[0].Calls[0].Call.Tracks = nil
			}
			require.Equal(t, "blocking-approval", customEvidence(r).Verdict)
		})
	}
}
func TestAsyncCapacityViewPreservesRawAudioAndVideoFailures(t *testing.T) {
	r := capacityFixture()
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGap: 3 * time.Second}, {Kind: "video", MediaGap: 2 * time.Second}}
	r.Yardstick = append(r.Yardstick, loadgen.MetricRow{Metric: "audio_freshness", Outcome: "fail"})
	view := asyncCapacityView(r)
	require.Len(t, r.Processes[0].Calls[0].Call.Tracks, 2)
	require.Len(t, view.Processes[0].Calls[0].Call.Tracks, 1)
	for _, row := range view.Yardstick {
		require.False(t, strings.HasPrefix(row.Metric, "audio_"))
	}
	require.Equal(t, "fail", judgeCapacityStep("worker", view, 100, 128<<10, nil, true).Verdict)
}
