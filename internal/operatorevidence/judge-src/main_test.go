package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

// Shape and thresholds from public internal/loadgen/distributed_test.go.
// Thirty serial events on ONE caller, not thirty concurrent calls.
func previewSample() loadgen.RunReport {
	baseline := make([]time.Duration, 50)
	for i := range baseline {
		baseline[i] = 20 * time.Millisecond
	}
	v := callharness.MetricVerdict{Trusted: true}
	call := &callharness.Report{Distributed: &callharness.DistributedRecording{RunID: "run-fixture"}, Tracks: []callharness.TrackReport{
		{Kind: "audio", Packets: 1000, MediaGap: 20 * time.Millisecond}, {Kind: "video", Packets: 1000, MediaGap: 33 * time.Millisecond, Video: &callharness.VideoReport{}},
	}}
	base := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	call.StartedAt = base
	call.FirstVideoSentAt = time.Second
	call.HungUpAt = 120 * time.Second
	call.SentVideo.Frames = 100
	for i := 0; i < 10; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		call.Distributed.Probes = append(call.Distributed.Probes, callharness.ClockProbe{CallerSend: at, CallerReceive: at.Add(time.Millisecond), ServerReceive: at, ServerSend: at})
	}
	events := []loadgen.EventResult{}
	for i := 0; i < 30; i++ {
		kind, event := "move", "move"
		if i >= 20 {
			kind, event = "takeover", "kill"
		}
		id := callharness.OperationIdentity{RunID: "run-fixture", OperationID: fmt.Sprint("event-", i), SessionID: "session", Worker: "w1", Epoch: uint64(i + 1)}
		m := callharness.MoveReport{Kind: kind, Identity: &id, Measurement: callharness.EventMeasurement{AudioLoss: v, ResumeVerdict: v, Audio: callharness.FreshnessReport{Verdict: v},
			VideoExpected: true, VideoLoss: v, Video: callharness.FreshnessReport{Verdict: v}, FirstContentVerdict: v, FirstNewContentVerdict: v, FirstLiveVerdict: v}, Distributed: &callharness.DistributedMeasurement{WindowOutcome: "pass", Outcome: "pass"}}
		for _, track := range []string{"audio", "video"} {
			m.Distributed.Gaps = append(m.Distributed.Gaps, callharness.DistributedGap{Kind: track, BaselineCertain: baseline, BaselinePossible: baseline, EventGap: callharness.DurationInterval{Lower: 20 * time.Millisecond, Upper: 20 * time.Millisecond}})
		}
		m.Start = time.Duration(40+2*i) * time.Second
		m.End = m.Start + time.Millisecond
		if kind == "takeover" {
			m.Distributed.KillToLive = &callharness.DurationInterval{Lower: time.Second, Upper: time.Second}
		}
		m.Distributed.Mapped = true
		m.Distributed.Evaluations = 1
		m.Distributed.FirstBaselines = []callharness.FirstBaselines{{Audio: callharness.FreshnessBaseline{Samples: 50, Median: 20 * time.Millisecond, P95: 20 * time.Millisecond}, Video: callharness.FreshnessBaseline{Samples: 50, Median: 20 * time.Millisecond, P95: 20 * time.Millisecond}}}
		m.Distributed.Start = callharness.TimeInterval{Earliest: base.Add(m.Start), Latest: base.Add(m.Start)}
		m.Distributed.End = callharness.TimeInterval{Earliest: base.Add(m.End), Latest: base.Add(m.End)}
		m.Distributed.Durations = map[string]callharness.DurationInterval{}
		for _, name := range []string{"resume", "first_content", "first_new_content", "first_live"} {
			m.Distributed.Durations[name] = callharness.DurationInterval{Lower: time.Second, Upper: time.Second}
		}
		call.Distributed.Armed = append(call.Distributed.Armed, id)
		m.Measurement.ObservedUntil = 120 * time.Second
		call.Moves = append(call.Moves, m)
		events = append(events, loadgen.EventResult{Event: loadgen.Event{Kind: event}, Affected: 1, Sessions: []string{"session"}, Identities: []callharness.OperationIdentity{id}})
	}
	return loadgen.RunReport{Callers: 1, Processes: []loadgen.ProcessReport{{Complete: true, Calls: []loadgen.Message{{Session: "session", Call: call}}}}, Events: events}
}
func TestPreviewMediaPass(t *testing.T) {
	t.Parallel()
	r := previewSample()
	if got := previewMedia(&r); got.Status != "passed" {
		t.Fatal(got, r.Causes, r.Distributed)
	}
}
func TestPreviewMissingInconclusive(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*loadgen.RunReport){
		func(r *loadgen.RunReport) { r.Processes = nil },
		func(r *loadgen.RunReport) { r.Processes[0].Complete = false },
		func(r *loadgen.RunReport) { r.Processes[0].Calls[0].Call.Distributed = nil },
		func(r *loadgen.RunReport) { r.Events = nil },
		func(r *loadgen.RunReport) { r.Processes[0].Calls[0].Call.Tracks = nil },
		func(r *loadgen.RunReport) { r.Processes[0].Calls[0].Call.Moves[0].Measurement.VideoExpected = false },
		func(r *loadgen.RunReport) { r.Processes[0].Calls[0].Call.Moves = nil },
	} {
		r := previewSample()
		change(&r)
		r.Outcome = "valid"
		if got := previewMedia(&r); got.Status != "inconclusive" {
			t.Fatal(got)
		}
	}
}
func TestPreviewKnownFailSurvivesMissing(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Processes[0].Complete = false
	r.Processes[0].Calls[0].Call.DecryptionFailures.AuthTag = 1
	if got := previewMedia(&r); got.Status != "failed" {
		t.Fatal(got)
	}
	r = previewSample()
	for i := 18; i < 20; i++ {
		r.Processes[0].Calls[0].Call.Moves[i].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
	}
	if got := previewMedia(&r); got.Status != "failed" {
		t.Fatal(got)
	}
}
func TestPreviewOwnershipLossSurvivesMissing(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Processes[0].Complete = false
	r.Processes[0].Calls[0].Call.Moves[0].Lost = true
	if got := previewMedia(&r); got.Status != "failed" {
		t.Fatal(got)
	}
}
func TestPreviewNoProcessFaultAsKill(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Events[0].Event.Kind = "relay-deploy"
	if got := previewMedia(&r); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}
func TestPreviewRawMissingCannotPass(t *testing.T) {
	t.Parallel()
	input := previewMediaInput{RunID: "run-fixture", Sessions: []string{"session"}, CallerJSONL: ""}
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}
func sampleProbes(r loadgen.RunReport) []registrationProbe {
	var out []registrationProbe
	for _, p := range r.Processes[0].Calls[0].Call.Distributed.Probes {
		out = append(out, registrationProbe{RTT: p.CallerReceive.Sub(p.CallerSend).Nanoseconds(), Send: p.CallerSend, Receive: p.CallerReceive, ServerReceive: p.ServerReceive, ServerSend: p.ServerSend})
	}
	return out
}
func TestPreviewRawIdentityJoin(t *testing.T) {
	t.Parallel()
	r := previewSample()
	input := rawPreviewInput(r, 0)
	if got := joinedPreviewMedia(&input); got.Status != "passed" {
		t.Fatal(got)
	}
	probes := input.Probes
	input.Probes = nil
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	input.Probes = probes
	input.Sessions = []string{"wrong"}
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	input.Sessions = []string{"session"}
	input.RunID = "run-other"
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}

func TestPreviewMissingStatsFieldsInconclusive(t *testing.T) {
	t.Parallel()
	r := previewSample()
	input := rawPreviewInput(r, 0)
	input.CallerJSONL = strings.Replace(input.CallerJSONL, `"cpu_percent_per_gomaxprocs":0,`, "", 1)
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	r.Processes[0].Calls[0].Call.DecryptionFailures.AuthTag = 1
	input = rawPreviewInput(r, 0)
	input.CallerJSONL = strings.Replace(input.CallerJSONL, `"cpu_percent_per_gomaxprocs":0,`, "", 1)
	if got := joinedPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
}

func TestPreviewDecodeErrorsInconclusive(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Processes[0].Calls[0].Call.Tracks[1].Video = &callharness.VideoReport{OnlineDecode: callharness.OnlineDecodeReport{Errors: 1}}
	if got := previewMedia(&r); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	r.Processes[0].Calls[0].Call.DecryptionFailures.AuthTag = 1
	if got := previewMedia(&r); got.Status != "failed" {
		t.Fatal(got)
	}
}
func TestPreviewServerLossReceipt(t *testing.T) {
	t.Parallel()
	r := previewSample()
	receipt := previewReceipt{Identity: *r.Processes[0].Calls[0].Call.Moves[0].Identity}
	receipt.Result.Lost = true
	input := previewMediaInput{RunID: "run-fixture", Sessions: []string{"session"}, Receipts: []previewReceipt{receipt}}
	if got := joinedPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
	input.Receipts[0].Identity.RunID = "foreign"
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}

func rawPreviewInput(r loadgen.RunReport, spike int) previewMediaInput {
	var stream strings.Builder
	encoder := json.NewEncoder(&stream)
	call := r.Processes[0].Calls[0].Call
	for i := 1; i <= 122; i++ {
		stats := &loadgen.Stats{At: call.StartedAt.Add(time.Duration(i) * time.Second), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 50}}
		if i == spike {
			stats.IntervalLateness.Max = 2 * time.Second
		}
		_ = encoder.Encode(loadgen.Message{Type: "stats", PID: 42, Stats: stats})
	}
	m := r.Processes[0].Calls[0]
	m.Type = "call"
	m.PID = 42
	_ = encoder.Encode(m)
	_ = encoder.Encode(loadgen.Message{Type: "done", PID: 42})
	input := previewMediaInput{RunID: "run-fixture", Sessions: []string{"session"}, Probes: sampleProbes(r), CallerJSONL: stream.String(), Events: r.Events}
	for _, m := range call.Moves {
		receipt := previewReceipt{Identity: *m.Identity, Kind: m.Kind, Start: call.StartedAt.Add(m.Start), End: call.StartedAt.Add(m.End)}
		receipt.Result.Kind = m.Kind
		receipt.Result.ID = m.Identity.SessionID
		receipt.Result.From = m.Identity.Worker
		input.Receipts = append(input.Receipts, receipt)
	}
	return input
}
func TestPreviewGeneratorExclusions(t *testing.T) {
	t.Parallel()
	r := previewSample()
	input := rawPreviewInput(r, 40)
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	input.Receipts[0].Result.Lost = true
	if got := joinedPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
	input = rawPreviewInput(r, 0)
	input.Receipts[0].Uncertain = true
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	input.Receipts = nil
	if got := joinedPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}

// Sequential callers may pool within one run; concurrency is not the sample count.
func TestPreviewPoolAcrossGates(t *testing.T) {
	t.Parallel()
	a, b := previewSample(), previewSample()
	a.Processes[0].Calls[0].Call.Moves = a.Processes[0].Calls[0].Call.Moves[:15]
	a.Events = a.Events[:15]
	a.Processes[0].Calls[0].Call.Distributed.Armed = a.Processes[0].Calls[0].Call.Distributed.Armed[:15]
	b.Processes[0].Calls[0].Call.Moves = b.Processes[0].Calls[0].Call.Moves[15:]
	b.Events = b.Events[15:]
	b.Processes[0].Calls[0].Call.Distributed.Armed = b.Processes[0].Calls[0].Call.Distributed.Armed[15:]
	b.Processes[0].Calls[0].Session = "second"
	for i := range b.Processes[0].Calls[0].Call.Moves {
		b.Processes[0].Calls[0].Call.Moves[i].Identity.SessionID = "second"
		b.Processes[0].Calls[0].Call.Distributed.Armed[i].SessionID = "second"
		b.Events[i].Sessions = []string{"second"}
		b.Events[i].Identities[0].SessionID = "second"
	}
	if previewMedia(&a).Status != "inconclusive" || previewMedia(&b).Status != "inconclusive" {
		t.Fatal("partial gates supplied minimum counts")
	}
	a.Callers = 2
	a.Processes = append(a.Processes, b.Processes...)
	a.Events = append(a.Events, b.Events...)
	if got := previewMedia(&a); got.Status != "passed" {
		t.Fatal(got, a.Causes, a.Distributed)
	}
	b.Processes[0].Calls[0].Call.Distributed.RunID = "foreign"
	if got := previewMedia(&a); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}

func buildRawPoolSample() previewPoolInput {
	a, b := previewSample(), previewSample()
	a.Processes[0].Calls[0].Call.Moves = a.Processes[0].Calls[0].Call.Moves[:15]
	a.Events = a.Events[:15]
	a.Processes[0].Calls[0].Call.Distributed.Armed = a.Processes[0].Calls[0].Call.Distributed.Armed[:15]
	b.Processes[0].Calls[0].Call.Moves = b.Processes[0].Calls[0].Call.Moves[15:]
	b.Events = b.Events[15:]
	b.Processes[0].Calls[0].Call.Distributed.Armed = b.Processes[0].Calls[0].Call.Distributed.Armed[15:]
	b.Processes[0].Calls[0].Session = "second"
	for i := range b.Processes[0].Calls[0].Call.Moves {
		b.Processes[0].Calls[0].Call.Moves[i].Identity.SessionID = "second"
		b.Processes[0].Calls[0].Call.Distributed.Armed[i].SessionID = "second"
		b.Events[i].Sessions = []string{"second"}
		b.Events[i].Identities[0].SessionID = "second"
	}
	left, right := rawPreviewInput(a, 0), rawPreviewInput(b, 0)
	right.Sessions = []string{"second"}
	return previewPoolInput{RunID: "run-fixture", Phases: []previewMediaInput{left, right}}
}

var rawPoolControl = sync.OnceValue(buildRawPoolSample)

func rawPoolSample() previewPoolInput {
	input := rawPoolControl()
	input.Phases = append([]previewMediaInput{}, input.Phases...)
	for i := range input.Phases {
		p := &input.Phases[i]
		p.Sessions = append([]string{}, p.Sessions...)
		p.Probes = append([]registrationProbe{}, p.Probes...)
		p.Receipts = append([]receipt{}, p.Receipts...)
		p.Events = append([]loadgen.EventResult{}, p.Events...)
		for j := range p.Events {
			p.Events[j].Sessions = append([]string{}, p.Events[j].Sessions...)
			p.Events[j].Identities = append([]callharness.OperationIdentity{}, p.Events[j].Identities...)
		}
	}
	return input
}

func TestPreviewRawPoolQualifiesOnce(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	for _, phase := range input.Phases {
		if joinedPreviewMedia(&phase).Status != "inconclusive" {
			t.Fatal("partial phase passed")
		}
	}
	if got := pooledPreviewMedia(&input); got.Status != "passed" || got.ExpectedCalls != 2 || got.EventCount != 30 || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 {
		t.Fatal(got)
	}
}
func TestPreviewSkippedSampleInconclusive(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	input.Phases = input.Phases[:1]
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
	input = rawPoolSample()
	input.Missing = true
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}
func TestPreviewRawPoolRejectsReplayAndForeignRun(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*previewPoolInput){
		func(p *previewPoolInput) { p.Phases = append(p.Phases, p.Phases[0]) },
		func(p *previewPoolInput) { p.Phases[1].RunID = "foreign" },
		func(p *previewPoolInput) { p.Phases[1].Sessions = p.Phases[0].Sessions },
		func(p *previewPoolInput) {
			p.Phases[0].Receipts = append(p.Phases[0].Receipts, p.Phases[0].Receipts[0])
		},
		func(p *previewPoolInput) { p.Phases[0].Events = append(p.Phases[0].Events, p.Phases[0].Events[0]) },
	} {
		input := rawPoolSample()
		change(&input)
		if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
			t.Fatal(got)
		}
	}
}
func TestPreviewRawPoolRetainsFailure(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	input.Missing = true
	input.Phases[0].Receipts[0].Result.Lost = true
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
	input = rawPoolSample()
	input.Phases[0].CallerJSONL = ""
	input.Phases[1].Receipts[0].Result.Lost = true
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
}
func TestPreviewRawPoolRejectsDuplicateMoves(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Processes[0].Calls[0].Call.Moves = append(r.Processes[0].Calls[0].Call.Moves, r.Processes[0].Calls[0].Call.Moves[0])
	phase := rawPreviewInput(r, 0)
	// Keep ONE real receipt and event for that operation. Only the caller replays it.
	phase.Receipts = phase.Receipts[:30]
	input := previewPoolInput{RunID: "run-fixture", Phases: []previewMediaInput{phase}}
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal(got)
	}
}
