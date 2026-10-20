package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/workerdriver"
	"github.com/relaishq/relais/pkg/callharness"
)

type customCapacityBuild struct {
	Worker          string         `json:"worker_binary"`
	SHA256          string         `json:"worker_sha256"`
	Agent           string         `json:"agent"`
	Approval        string         `json:"approval"`
	Workload        string         `json:"workload_interpretation"`
	OperatorBudgets map[string]int `json:"operator_budgets"`
}
type customCapacityEvidence struct {
	CheckpointMaxMS                                 float64            `json:"checkpoint_age_max_ms"`
	SnapshotMaxMS                                   float64            `json:"snapshot_age_max_ms"`
	AdmissionCalls                                  float64            `json:"admission_calls"`
	AdmissionWallSeconds                            float64            `json:"admission_wall_seconds"`
	AdmissionCPUSeconds                             float64            `json:"admission_cpu_seconds"`
	AdmissionCPUUnavailable                         float64            `json:"admission_cpu_unavailable"`
	ForcedWallSeconds                               float64            `json:"forced_wall_seconds"`
	ForcedMaxMS                                     float64            `json:"forced_max_ms"`
	Skips                                           map[string]float64 `json:"checkpoint_skips"`
	Verdict                                         string             `json:"evidence_verdict"`
	Blocking                                        []string           `json:"blocking_approval,omitempty"`
	AgeSamples                                      int                `json:"age_samples"`
	CheckpointP99MS                                 float64            `json:"checkpoint_age_p99_ms"`
	SnapshotP99MS                                   float64            `json:"snapshot_age_p99_ms"`
	Events, Saves                                   float64
	SaveWallSecondsPerEvent                         float64 `json:"save_wall_seconds_per_event"`
	SaveCPUSecondsPerEvent                          float64 `json:"save_cpu_seconds_per_event"`
	SaveWallSecondsPerCallSecond                    float64 `json:"save_wall_seconds_per_call_second"`
	SaveCPUSecondsPerCallSecond                     float64 `json:"save_cpu_seconds_per_call_second"`
	SaveCPUUnavailable                              float64 `json:"save_cpu_unavailable"`
	ForcedAttempts, ForcedSuccesses, ForcedFailures float64
	SnapshotDebitBytes                              float64 `json:"snapshot_debit_bytes"`
	ExportHTTPPeakBytes                             float64 `json:"export_http_peak_bytes"`
	MoveReservationPeakBytes                        int64   `json:"move_reservation_peak_bytes"`
	Note                                            string  `json:"note"`
}

func customCapacity(c config) bool {
	return workerdriver.IsCustom(c.Worker.Agent)
}
func customBuild(c config) (*customCapacityBuild, error) {
	binary := c.Worker.Path(c.Bin)
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return &customCapacityBuild{Worker: binary, SHA256: hex.EncodeToString(hash[:]), Agent: c.Worker.Agent, Approval: "Evidence for owner approval only; approval binds to this exact build and operator budgets. Over-budget or incomplete evidence blocks approval.", Workload: "Continuous harness Opus input plus VP8 video; delayed-loopback records up to 2s / 64KiB and replies in bounded chunks after 300ms. 100 commands and 128KiB Redis request+reply per call-second are upper budgets, not artificial traffic to generate. No provider load is simulated. Echo audio loss/freshness/gap checks do not apply to delayed playback; raw results remain attached. Video, connection, move, drain and takeover checks remain enforced.", OperatorBudgets: map[string]int{"max_snapshot_bytes": 7168, "snapshot_bytes_per_second": 81920, "snapshot_burst_bytes": 28672, "move_bytes_per_session": 1048576, "move_bytes_total": 67108864, "checkpoint_age_p99_ms": 550, "snapshot_age_p99_ms": 550}}, nil
}

// The async app intentionally pauses audio. Keep the complete raw report, but
// do not use echo timing as an application audio-continuity verdict. Video and
// transport checks still apply. Exact audio preservation has separate tests.
func asyncCapacityView(raw loadgen.RunReport) loadgen.RunReport {
	view := raw
	view.Telemetry = raw.Telemetry
	view.Telemetry.Processes = slices.Clone(raw.Telemetry.Processes)
	for i, p := range view.Telemetry.Processes {
		rates := map[string]float64{}
		for name, rate := range p.RatesPerCallSecond {
			if strings.HasPrefix(name, "relais_store_commands_total") {
				continue
			}
			if strings.HasPrefix(name, "relais_store_wire_commands_total") {
				name = "relais_store_commands_total" + strings.TrimPrefix(name, "relais_store_wire_commands_total")
			}
			rates[name] = rate
		}
		view.Telemetry.Processes[i].RatesPerCallSecond = rates
	}
	view.Yardstick = slices.DeleteFunc(slices.Clone(raw.Yardstick), func(row loadgen.MetricRow) bool { return strings.HasPrefix(row.Metric, "audio_") })
	view.Processes = slices.Clone(raw.Processes)
	for i := range view.Processes {
		view.Processes[i].Calls = slices.Clone(raw.Processes[i].Calls)
		for j, c := range view.Processes[i].Calls {
			if c.Call == nil {
				continue
			}
			report := *c.Call
			report.Tracks = slices.DeleteFunc(slices.Clone(report.Tracks), func(t callharness.TrackReport) bool { return t.Kind == "audio" })
			report.Moves = slices.Clone(report.Moves)
			for k := range report.Moves {
				report.Moves[k].Tracks = slices.DeleteFunc(slices.Clone(report.Moves[k].Tracks), func(t callharness.MoveTrackReport) bool { return t.Kind == "audio" })
			}
			view.Processes[i].Calls[j].Call = &report
		}
	}
	return view
}
func customEvidence(r loadgen.RunReport) *customCapacityEvidence {
	e := &customCapacityEvidence{Verdict: "pass", Note: "Save time measures application Save only; Linux thread CPU is separate from wall time. Events count Handle invocations including refused callbacks. Forced intent attempts include store-clock failures; raw per-client command and wire counters include the existing retry policy. Export is the largest actual JSON HTTP response body, not pending memory. Move reservations, worker heap/RSS and HTTP bytes are separate; see telemetry. Sampled current ages every 100ms, not a takeover-only histogram."}
	block := func(reason string) { e.Verdict = "blocking-approval"; e.Blocking = append(e.Blocking, reason) }
	ages := r.Telemetry.Agents
	var checkpoints, snapshots []float64
	coverage := map[string]int{}
	for _, s := range ages.Samples {
		coverage[s.Status.ID]++
		e.CheckpointMaxMS = max(e.CheckpointMaxMS, s.Status.CheckpointAgeMS)
		e.SnapshotMaxMS = max(e.SnapshotMaxMS, s.Status.SnapshotAgeMS)
		checkpoints = append(checkpoints, s.Status.CheckpointAgeMS)
		snapshots = append(snapshots, s.Status.SnapshotAgeMS)
		e.MoveReservationPeakBytes = max(e.MoveReservationPeakBytes, s.Status.PendingBytes)
	}
	e.AgeSamples = len(checkpoints)
	p99 := func(values []float64) float64 {
		if len(values) == 0 {
			return 0
		}
		slices.Sort(values)
		return values[int(math.Ceil(.99*float64(len(values))))-1]
	}
	e.CheckpointP99MS, e.SnapshotP99MS = p99(checkpoints), p99(snapshots)
	ticks := r.Duration.Seconds() * 10
	minimum := max(1, int(math.Floor(ticks*.9)))
	expected := map[string]bool{}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			expected[c.Session] = true
		}
	}
	delete(expected, "")
	if len(expected) != r.Callers {
		block("missing per-session age coverage identities")
	}
	for id := range expected {
		if coverage[id] < minimum {
			block("incomplete 100ms age coverage for session " + id)
		}
	}
	if ages.Missing > uint64(math.Ceil(float64(r.Callers)*ticks*.1)) {
		block("more than 10% missing session age samples")
	}
	if len(checkpoints) < r.Callers*minimum || len(ages.Errors) > 0 {
		block("missing or incomplete 100ms age samples")
	}
	if e.CheckpointP99MS > 550 || e.SnapshotP99MS > 550 {
		block("checkpoint-age or snapshot-age p99 exceeds 550ms")
	}
	delta := func(p int, name string) float64 {
		v := r.Telemetry.Processes[p]
		first, fOK := v.First[name]
		last, lOK := v.Last[name]
		if !fOK || !lOK || last < first {
			block("missing or reset worker counter " + name)
			return 0
		}
		return last - first
	}
	var wall, cpu float64
	e.Skips = map[string]float64{}
	optionalDelta := func(p capacity.Process, name string) float64 { return max(0, p.Last[name]-p.First[name]) }
	for _, p := range r.Telemetry.Processes {
		for _, counters := range []map[string]float64{p.First, p.Last} {
			for name, count := range counters {
				if strings.HasPrefix(name, "relais_store_wire_parse_failures_total") && count > 0 {
					block("Redis wire command accounting failed")
				}
			}
		}
	}
	for i, p := range r.Telemetry.Processes {
		if !strings.HasPrefix(p.Name, "worker-") {
			continue
		}
		e.AdmissionCalls += optionalDelta(p, "relais_worker_agent_admission_calls_total")
		e.AdmissionWallSeconds += optionalDelta(p, "relais_worker_agent_admission_wall_nanoseconds_total") / 1e9
		e.AdmissionCPUSeconds += optionalDelta(p, "relais_worker_agent_admission_cpu_nanoseconds_total") / 1e9
		e.AdmissionCPUUnavailable += optionalDelta(p, "relais_worker_agent_admission_cpu_unavailable_total")
		e.ForcedWallSeconds += optionalDelta(p, "relais_worker_agent_forced_checkpoint_wall_nanoseconds_total") / 1e9
		e.ForcedMaxMS = max(e.ForcedMaxMS, p.Peaks["relais_worker_agent_forced_checkpoint_max_nanoseconds"]/1e6)
		for _, reason := range []string{"budget", "interval"} {
			e.Skips[reason] += optionalDelta(p, "relais_worker_agent_checkpoint_"+reason+"_skips_total")
		}
		e.Events += delta(i, "relais_worker_agent_events_total")
		e.Saves += delta(i, "relais_worker_agent_save_calls_total")
		wall += delta(i, "relais_worker_agent_save_wall_nanoseconds_total") / 1e9
		cpu += delta(i, "relais_worker_agent_save_cpu_nanoseconds_total") / 1e9
		e.SaveCPUUnavailable += delta(i, "relais_worker_agent_save_cpu_unavailable_total")
		e.ForcedAttempts += delta(i, "relais_worker_agent_forced_checkpoint_attempts_total")
		e.ForcedSuccesses += delta(i, "relais_worker_agent_forced_checkpoint_successes_total")
		e.ForcedFailures += delta(i, "relais_worker_agent_forced_checkpoint_failures_total")
		e.SnapshotDebitBytes += delta(i, "relais_worker_agent_snapshot_debit_bytes_total")
		e.ExportHTTPPeakBytes = max(e.ExportHTTPPeakBytes, p.Peaks["relais_worker_export_http_peak_bytes"])
	}
	if e.Events <= 0 || e.Saves <= 0 || e.SaveCPUUnavailable > 0 {
		block("Save CPU/time incomplete (thread CPU requires Linux)")
	}
	if e.Events > 0 {
		e.SaveWallSecondsPerEvent, e.SaveCPUSecondsPerEvent = wall/e.Events, cpu/e.Events
	}
	denom := r.Telemetry.WindowSeconds * float64(r.Callers)
	if denom > 0 {
		e.SaveWallSecondsPerCallSecond, e.SaveCPUSecondsPerCallSecond = wall/denom, cpu/denom
	}
	if e.ExportHTTPPeakBytes <= 0 || e.ExportHTTPPeakBytes >= 4<<20 {
		block("actual export HTTP body absent or exceeds private API bound")
	}
	audioCalls := 0
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call != nil {
				for _, track := range c.Call.Tracks {
					if track.Kind == "audio" && track.Packets > 0 {
						audioCalls++
					}
				}
			}
		}
	}
	if audioCalls != r.Callers {
		block(fmt.Sprintf("returned custom audio missing for %d/%d calls", r.Callers-audioCalls, r.Callers))
	}
	return e
}

// Stock agents have no v2 diagnostics and must incur no private agent sampling.
func configureAgentSamples(c config, targets []capacity.Target, generators []generator) {
	if !workerdriver.IsCustom(c.Worker.Agent) && c.Study == nil {
		return
	}
	for i := range targets {
		if strings.HasPrefix(targets[i].Name, "worker-") {
			targets[i].CheckpointOnly = !workerdriver.IsCustom(c.Worker.Agent)
			for _, g := range generators {
				targets[i].AgentSessions = append(targets[i].AgentSessions, g.sessions...)
			}
		}
	}
}
