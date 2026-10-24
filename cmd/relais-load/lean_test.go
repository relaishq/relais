package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func leanPtr[T any](v T) *T { return &v }

// This fixture models serial external runs with real offset intervals and
// durable receipts. It never converts a local report into distributed proof.
// Fixed worker names model the deployed template; replacement generations
// change independently. Every phase has a fresh session, probes and arm/receipt
// brackets, generation-tagged observations and complete per-second samples.
func leanFixture(t *testing.T) leanManifest {
	t.Helper()
	m := leanManifest{Version: 1, RunID: "synthetic-run", Signaling: "https://signal.example/calls", Relay: "192.0.2.1:9999", Region: "test-region", Build: "synthetic-build", Agent: "echo", Payload: "audio/opus synthetic", StoreDurability: "sync", StoreTLS: true, RelayBufferEnabled: leanPtr(true), Clock: &leanClock{ControlBound: time.Millisecond, WorkerBound: time.Millisecond, DriftPPM: 0, Evidence: "synthetic independent host offsets +/-1ms"}}
	for i := 0; i < 4; i++ {
		m.Callers = append(m.Callers, leanCaller{ID: fmt.Sprint(i), Runner: fmt.Sprint("outside-runner-", i), Region: "caller-region", Bind: "192.0.2.2:0"})
	}
	registry := []leanWorker{{Name: "a", Generation: "a-generation", Ready: true}, {Name: "b", Generation: "b-generation", Ready: true}}
	for i := 0; i < 33; i++ {
		kind := "move"
		if i >= 20 && i < 30 {
			kind = "kill"
		}
		if i >= 30 && i < 32 {
			kind = "drain"
		}
		if i == 32 {
			kind = "boundary"
		}
		at := time.Unix(10000+int64(i)*40, 0).UTC()
		p := leanPhase{ID: fmt.Sprint("phase-", i), Kind: kind, CompletedAt: at.Add(31 * time.Second), WindowEvidence: "synthetic common collector-clock window, +/-1ms", WindowError: time.Millisecond}
		n := 1
		if kind == "boundary" {
			n = 4
		}
		p.Run = loadgen.RunReport{StartedAt: at, Duration: 30 * time.Second, Callers: n, Limits: loadgen.DefaultLimits()}
		p.Before = copyLeanPool(leanPool{At: at.Add(-10 * time.Millisecond), Cap: 2, Workers: registry})
		for slot := range p.Before.Workers {
			p.Before.Workers[slot].Sessions = nil
		}
		for j := 0; j < n; j++ {
			worker := 0
			if j >= 2 {
				worker = 1
			}
			p.Before.Workers[worker].Sessions = append(p.Before.Workers[worker].Sessions, fmt.Sprintf("s-%d-%d", i, j))
			p.CallerIDs = append(p.CallerIDs, fmt.Sprint(j))
		}
		p.After = copyLeanPool(p.Before)
		p.After.At = at.Add(10 * time.Second)
		if kind != "boundary" {
			p.After.Workers[0].Sessions = nil
			p.After.Workers[1].Sessions = []string{fmt.Sprintf("s-%d-0", i)}
		}
		if kind == "kill" || kind == "drain" {
			p.After.Workers[0].Ready = false
		}
		p.Restored = copyLeanPool(p.After)
		p.Restored.At = at.Add(29 * time.Second)
		p.Restored.Workers[0].Ready = true
		if kind == "kill" {
			p.Restored.Workers[0].Generation += "-replacement"
		}
		for j := 0; j < n; j++ {
			session := fmt.Sprintf("s-%d-%d", i, j)
			tracker, err := callharness.NewClockTracker(0)
			require.NoError(t, err)
			recording := &callharness.DistributedRecording{RunID: m.RunID, ControlOffset: callharness.OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond}, WorkerOffset: callharness.OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond}}
			for _, offset := range []time.Duration{0, 20 * time.Second} {
				probe := callharness.ClockProbe{CallerSend: at.Add(offset), CallerReceive: at.Add(offset + 2*time.Millisecond), ServerReceive: at.Add(offset + time.Millisecond), ServerSend: at.Add(offset + time.Millisecond)}
				require.NoError(t, tracker.Add(probe))
				recording.Probes = append(recording.Probes, probe)
			}
			c := &callharness.Report{RemoteAddr: m.Relay, StartedAt: at, ConnectedAt: time.Millisecond, HungUpAt: 30 * time.Second, OfferAnswerExchanges: 1, SentAudio: callharness.SentTrack{Frames: 1500}, Distributed: recording, Recording: callharness.RecordingReport{History: 180 * time.Second, Delivery: &callharness.WholeCallDelivery{Audio: callharness.ContentDelivery{Sent: 1500, Returned: 1500}}}, ConnectionStates: []callharness.StateChange{{State: "connected", At: time.Millisecond}}}
			c.Tracks = []callharness.TrackReport{{Kind: "audio", Packets: 1500, MediaGap: 20 * time.Millisecond, MediaGapEndedAt: time.Second, MediaGapsRecorded: true}}
			if kind != "boundary" {
				id := callharness.OperationIdentity{RunID: m.RunID, OperationID: p.ID, SessionID: session, Worker: p.Before.Workers[0].Name, Epoch: uint64(i + 1)}
				moveKind := "move"
				if kind == "kill" {
					moveKind = "takeover"
				}
				receipt := &callharness.DistributedEvent{Identity: id, Kind: moveKind, Start: at.Add(6 * time.Second), End: at.Add(6100 * time.Millisecond), Result: controlplane.MoveResult{ID: session, Kind: moveKind, From: p.Before.Workers[0].Name, To: p.Before.Workers[1].Name, Start: at.Add(6 * time.Second), End: at.Add(6100 * time.Millisecond)}}
				start, err := tracker.Map(receipt.Start, recording.ControlOffset)
				require.NoError(t, err)
				end, err := tracker.Map(receipt.End, recording.ControlOffset)
				require.NoError(t, err)
				trusted := callharness.MetricVerdict{Trusted: true}
				baseline := make([]time.Duration, 50)
				for k := range baseline {
					baseline[k] = 20 * time.Millisecond
				}
				d := &callharness.DistributedMeasurement{Mapped: true, WindowOutcome: "pass", Outcome: "pass", Start: start, End: end, AudioLoss: trusted, VideoLoss: trusted, Durations: map[string]callharness.DurationInterval{"audio_freshness": {Lower: 100 * time.Millisecond, Upper: 104 * time.Millisecond}, "resume": {Lower: 100 * time.Millisecond, Upper: 104 * time.Millisecond}}, Gaps: []callharness.DistributedGap{{Kind: "audio", BaselineCertain: baseline, BaselinePossible: baseline, EventGap: callharness.DurationInterval{Lower: 20 * time.Millisecond, Upper: 20 * time.Millisecond}}}}
				if kind == "kill" {
					d.KillToLive = &callharness.DurationInterval{Lower: 100 * time.Millisecond, Upper: 104 * time.Millisecond}
				}
				c.Moves = []callharness.MoveReport{{Identity: &id, Receipt: receipt, Kind: moveKind, Distributed: d, Measurement: callharness.EventMeasurement{AudioLoss: trusted, Audio: callharness.FreshnessReport{Verdict: trusted}, ResumeVerdict: trusted}}}
				recording.Armed = []callharness.OperationIdentity{id}
				p.Run.Events = []loadgen.EventResult{{Event: loadgen.Event{Kind: kind, After: 6 * time.Second}, Affected: 1, Sessions: []string{session}, Identities: []callharness.OperationIdentity{id}, At: receipt.Start, FinishedAt: receipt.End}}
				p.Timings = []leanTiming{{Identity: id, ArmAcknowledgedAt: at.Add(5 * time.Second), ReceiptSentAt: at.Add(7 * time.Second)}}
			}
			proc := loadgen.ProcessReport{PID: 100 + j, ExpectedCalls: 1, Complete: true, Lateness: loadgen.Lateness{Packets: 1500, Max: time.Millisecond}, Calls: []loadgen.Message{{Type: "call", PID: 100 + j, Session: session, Call: c}}}
			for k := 1; k <= 30; k++ {
				proc.Samples = append(proc.Samples, loadgen.Stats{At: at.Add(time.Duration(k) * time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 5, IntervalLateness: loadgen.Lateness{Packets: 50, P99: time.Millisecond, Max: time.Millisecond}, Lateness: loadgen.Lateness{Packets: uint64(k * 50), Max: time.Millisecond}, IntervalPacing: loadgen.Pacing{SentFrames: 50}})
			}
			p.Run.Processes = append(p.Run.Processes, proc)
			for k := 0; k < 300; k++ {
				owner := leanWorkerFor(p.Before, session)
				if kind != "boundary" && k >= 61 {
					owner = leanWorkerFor(p.After, session)
				}
				p.Run.Telemetry.Agents.Samples = append(p.Run.Telemetry.Agents.Samples, capacity.AgentSample{At: at.Add(time.Duration(k) * 100 * time.Millisecond), Worker: owner.Name, Status: mediaworker.AgentSessionStatus{ID: session, CheckpointKnown: true, CheckpointAgeMS: 100, SnapshotAgeMS: 100}})
			}
		}
		if kind == "drain" {
			p.DrainEmpty = &leanEmpty{Worker: p.Before.Workers[0].Name, At: at.Add(10 * time.Second)}
		}
		if kind == "boundary" {
			p.Admission = &leanAdmission{Status: 503, Code: "at_capacity", RetryAfter: "1", Before: copyLeanPool(p.Before), After: copyLeanPool(p.Before), RefusalsAfter: 1}
		}
		p.Run.Telemetry.WindowSeconds = 30
		p.Run.Telemetry.Agents.IntervalMS = 100
		for _, name := range []string{"relay", "control", "store", "worker-" + p.Before.Workers[0].Name + "@" + p.Before.Workers[0].Generation, "worker-" + p.Before.Workers[1].Name + "@" + p.Before.Workers[1].Generation} {
			proc := capacity.Process{Name: name, PID: 7, Samples: 30, ObservedSeconds: 30, CPUSeconds: 1, PeakRSSBytes: 100, First: map[string]float64{}, Last: map[string]float64{}, Peaks: map[string]float64{}}
			counter := func(key string, d float64) { proc.First[key] = 0; proc.Last[key] = d }
			counter(`relais_store_commands_total{client="session"}`, 0)
			counter(`relais_transport_bytes_total{client="session",transport="redis",direction="out"}`, 0)
			if name == "relay" {
				counter("relais_relay_worker_packets_total", float64(n)*1500)
				counter("relais_relay_caller_packets_total", float64(n)*1500)
				counter("relais_relay_public_udp_bytes_out_total", float64(n)*300000)
				proc.Last["relais_relay_buffer_enabled"] = 1
			}
			if name == "control" {
				value := 0.0
				if kind == "kill" {
					value = 1
				}
				counter("relais_control_takeovers_total", value)
				for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
					counter(`relais_control_losses_total{cause="`+cause+`"}`, 0)
				}
				proc.Last["relais_control_worker_call_cap"] = 2
			}
			if strings.HasPrefix(name, "worker-") {
				counter("relais_worker_decryption_failures_total", 0)
				counter("relais_worker_replay_rejections_total", 0)
				counter(`relais_store_commands_total{client="session"}`, float64(n)*30*20/2)
				counter(`relais_transport_bytes_total{client="session",transport="redis",direction="out"}`, float64(n)*30*1024/2)
				proc.Peaks["relais_worker_active_sessions"] = float64(min(n, 2))
			}
			p.Run.Telemetry.Processes = append(p.Run.Telemetry.Processes, proc)
		}
		if kind == "boundary" {
			first := p.Run.Processes[0]
			first.ExpectedCalls = 4
			first.Lateness.Packets *= 4
			for index := range first.Samples {
				first.Samples[index].Lateness.Packets *= 4
				first.Samples[index].IntervalLateness.Packets *= 4
				first.Samples[index].IntervalPacing.SentFrames *= 4
			}
			for index := 1; index < len(p.Run.Processes); index++ {
				message := p.Run.Processes[index].Calls[0]
				message.PID = first.PID
				message.CallID = index
				first.Calls = append(first.Calls, message)
			}
			p.Run.Processes = []loadgen.ProcessReport{first}
			p.CallerIDs = p.CallerIDs[:1]
		}
		m.Phases = append(m.Phases, p)
		registry = copyLeanPool(p.Restored).Workers
	}
	m.Cleanup = loadgen.LeakReport{Checked: true, ScanControls: map[string]bool{"session_epoch": true, "relay-fencing-hash": true}, RelayWait: soakRelayWait, RelayObservedAt: time.Unix(20000, 0)}
	return m
}
func copyLeanPool(p leanPool) leanPool {
	out := p
	out.Workers = append([]leanWorker(nil), p.Workers...)
	for i := range out.Workers {
		out.Workers[i].Sessions = append([]string(nil), p.Workers[i].Sessions...)
	}
	return out
}
func leanCauses(r leanReport) string { return strings.Join(r.Causes, "; ") }

func TestLeanDistributedPolicy(t *testing.T) {
	m := leanFixture(t)
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	require.NotNil(t, r.Trials)
	require.Equal(t, "pass", r.Trials.Outcome)
	require.Equal(t, "NOT-RUN", r.Cost.Status)
	require.NotEmpty(t, r.Cost.Unknown)
	require.False(t, r.FullSoakAcceptance)
	require.Equal(t, 2400, leanPlan().ExpectedSeconds)
	require.Less(t, r.Schedule.ExpectedSeconds, 3600)
	require.NotNil(t, r.Evidence.Phases[0].Run.Processes[0].Calls[0].Call.Moves[0].Receipt)
}
func TestLeanFailuresAndUnknowns(t *testing.T) {
	tests := []struct {
		name, want, contains string
		change               func(*leanManifest)
	}{
		{"store commands", "fail", "store command/byte budget", func(m *leanManifest) {
			p := &m.Phases[0].Run.Telemetry.Processes[3]
			for key := range p.Last {
				if strings.HasPrefix(key, "relais_store_commands") {
					p.Last[key] = 6000
				}
			}
		}},
		{"store bytes", "fail", "store command/byte budget", func(m *leanManifest) {
			p := &m.Phases[0].Run.Telemetry.Processes[3]
			for key := range p.Last {
				if strings.Contains(key, "transport_bytes") {
					p.Last[key] = 30 * 256 * 1024
				}
			}
		}},
		{"cleanup key unknown", "inconclusive", "cleanup key retention unavailable", func(m *leanManifest) {
			m.Cleanup.Keys = []loadgen.RetainedKey{{Key: "session-key", Class: "session", Error: "read deadline"}}
			m.Cleanup.StoreKeys = []string{"session-key"}
		}},
		{"wrong destination", "inconclusive", "receipt destination", func(m *leanManifest) {
			m.Phases[0].Run.Processes[0].Calls[0].Call.Moves[0].Receipt.Result.To = "unknown"
		}},
		{"premature after observation", "inconclusive", "precedes completed operation", func(m *leanManifest) { m.Phases[0].After.At = m.Phases[0].Run.StartedAt.Add(time.Second) }},
		{"wrong relay", "inconclusive", "media endpoint", func(m *leanManifest) { m.Phases[0].Run.Processes[0].Calls[0].Call.RemoteAddr = "127.0.0.1:9999" }},
		{"clock", "inconclusive", "clock", func(m *leanManifest) {
			m.Phases[0].Run.Processes[0].Calls[0].Call.Distributed.ClockError = "clock jump"
		}},
		{"clock and loss", "fail", "lost", func(m *leanManifest) {
			c := m.Phases[0].Run.Processes[0].Calls[0].Call
			c.Distributed.ClockError = "clock jump"
			c.Moves[0].Receipt.Result.Lost = true
		}},
		{"unknown checkpoint", "inconclusive", "age coverage", func(m *leanManifest) {
			for i := range m.Phases[0].Run.Telemetry.Agents.Samples {
				m.Phases[0].Run.Telemetry.Agents.Samples[i].Status.CheckpointKnown = false
			}
		}},
		{"age tail", "fail", "age p99", func(m *leanManifest) {
			for i := range m.Phases[0].Run.Telemetry.Agents.Samples {
				m.Phases[0].Run.Telemetry.Agents.Samples[i].Status.CheckpointAgeMS = 600
			}
		}},
		{"receipt missing", "inconclusive", "receipt", func(m *leanManifest) { m.Phases[0].Run.Processes[0].Calls[0].Call.Moves[0].Receipt = nil }},
		{"too few trials", "inconclusive", "20 move", func(m *leanManifest) { m.Phases = m.Phases[1:] }},
		{"admission accepted", "fail", "cleanly refused", func(m *leanManifest) { m.Phases[32].Admission.Status = 200 }},
		{"admission leaks", "fail", "reservations", func(m *leanManifest) { m.Phases[32].Admission.After.Workers[0].Reservations = 1 }},
		{"ENA missing", "inconclusive", "ENA", func(m *leanManifest) { m.ENARequired = true }},
		{"ENA drops", "fail", "ENA allowance", func(m *leanManifest) {
			m.Phases[0].Allowances = []leanAllowance{{Host: "relay", Generation: "1", First: map[string]float64{}, Last: map[string]float64{}}}
			for _, key := range leanENACounters {
				m.Phases[0].Allowances[0].First[key] = 0
				m.Phases[0].Allowances[0].Last[key] = 1
			}
		}},
		{"generator", "inconclusive", "saturation", func(m *leanManifest) { m.Phases[0].Run.Processes[0].Samples[0].CPUPerGOMAXPROCS = 90 }},
		{"resource missing", "inconclusive", "resource", func(m *leanManifest) { m.Phases[0].Run.Telemetry.Processes = nil }},
		{"lost content", "fail", "loss outside", func(m *leanManifest) {
			d := &m.Phases[0].Run.Processes[0].Calls[0].Call.Recording.Delivery.Audio
			d.Lost = 1
			d.Returned--
			d.LostAt = []time.Duration{time.Second}
		}},
		{"unscheduled gap", "fail", "unscheduled", func(m *leanManifest) {
			c := m.Phases[0].Run.Processes[0].Calls[0].Call
			c.Tracks[0].MediaGap = 500 * time.Millisecond
			c.Tracks[0].MediaGapEndedAt = 20 * time.Second
		}},
		{"phase caps", "fail", "occupancy", func(m *leanManifest) {
			m.Phases[0].Run.Telemetry.Processes[3].Peaks["relais_worker_active_sessions"] = 2
		}},
		{"drain proof", "inconclusive", "empty proof", func(m *leanManifest) { m.Phases[30].DrainEmpty = nil }},
		{"refusal wrong code", "fail", "cleanly refused", func(m *leanManifest) { m.Phases[32].Admission.Code = "no_target" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := leanFixture(t)
			tc.change(&m)
			r := judgeLean(m)
			require.Equal(t, tc.want, r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), tc.contains)
		})
	}
}
func TestLeanEntryIsSeparate(t *testing.T) {
	for _, args := range [][]string{{"-lean-plan", "-capacity"}, {"-lean-plan", "-soak"}, {"-lean-plan", "-external-caller"}, {"-lean-plan", "-workers=2"}, {"-lean-capacity="}} {
		require.Error(t, runArgs(args))
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, runArgs([]string{"-lean-plan", "-output=" + path}))
	var plan leanSchedule
	require.NoError(t, readLeanJSON(path, &plan))
	require.Equal(t, 2400, plan.ExpectedSeconds)
	m := leanFixture(t)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	input := filepath.Join(t.TempDir(), "input.json")
	output := filepath.Join(t.TempDir(), "report.json")
	require.NoError(t, os.WriteFile(input, b, 0600))
	require.NoError(t, runArgs([]string{"-lean-capacity=" + input, "-output=" + output}))
	var r leanReport
	require.NoError(t, readLeanJSON(output, &r))
	require.Equal(t, "pass", r.Outcome)
	m.Phases[0].Run.Processes[0].Calls[0].Call.Distributed.ClockError = "clock jump"
	b, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, b, 0600))
	require.Error(t, runArgs([]string{"-lean-capacity=" + input, "-output=" + output}))
	require.NoError(t, readLeanJSON(output, &r))
	require.Equal(t, "inconclusive", r.Outcome)
}
