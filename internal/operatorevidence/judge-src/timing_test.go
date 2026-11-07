package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

func TestPaidMediaTimingRemainsStrict(t *testing.T) {
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		c.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{"first content classification changes within event-time interval; width=1ms"}}
	})
	got := pooledPreviewMedia(&input)
	if got.Status != "inconclusive" || got.InconclusiveMetrics != 2 || got.FailedMetrics != 0 || !got.CompleteMedia || len(got.UndecidedMetrics) != 2 || got.DroppedPhases != 0 {
		t.Fatal("strict paid judgment changed", got)
	}
	for i, row := range got.UndecidedMetrics {
		if row.Phase != i || row.Event != 0 || row.Metric != "first_content" || len(row.Causes) != 1 || row.Causes[0] != "classification-varies-with-clock-interval" {
			t.Fatal("wrong timing diagnostic", row)
		}
	}
	input.Phases[1].Receipts[0].Result.Lost = true
	got = pooledPreviewMedia(&input)
	if got.Status != "failed" || got.LostCalls != 1 {
		t.Fatal("loss hidden by undecided timing", got)
	}
}

func TestUndecidedRowsNameEveryMetricAndCause(t *testing.T) {
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		c.Moves[0].Measurement.FirstNewContentVerdict = callharness.MetricVerdict{Reasons: []string{"interval evaluation unavailable or exceeds 4096 evaluations; width=1ms", "private-secret"}}
		c.Moves[0].Distributed.WindowOutcome = "inconclusive"
		c.Moves[0].Distributed.Causes = []string{"event window evidence incomplete"}
	})
	got := pooledPreviewMedia(&input)
	if len(got.UndecidedMetrics) != got.InconclusiveMetrics || got.InconclusiveMetrics < 30 {
		t.Fatal("undecided rows lost", got)
	}
	for _, row := range got.UndecidedMetrics {
		if row.Phase < 0 || row.Phase > 1 || row.Event < 0 || row.Event >= 15 || len(row.Causes) == 0 {
			t.Fatal(row)
		}
		for _, cause := range row.Causes {
			if cause == "private-secret" {
				t.Fatal("raw reason escaped")
			}
		}
	}
}

func freeRehearsalTimingInput(reason string) previewPoolInput {
	input := previewPoolInput{PreCoverageEvidence: true, RunID: "run-fixture"} // Frozen timing-only evidence predates coverage.
	for i, gate := range operatorrehearsal.CoveredGates {
		phase := relayInput()
		if i < 3 {
			r := previewSample()
			r.Processes[0].Calls[0].Call.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{reason}}
			phase = rawPreviewInput(r, 0)
		}
		phase.Gate = string(gate)
		session := fmt.Sprint("phase-", i)
		phase.Sessions = []string{session}
		phase.CallerJSONL = strings.ReplaceAll(phase.CallerJSONL, `:"session"`, `:"`+session+`"`)
		for j := range phase.Events {
			phase.Events[j].Sessions = []string{session}
			for k := range phase.Events[j].Identities {
				phase.Events[j].Identities[k].SessionID = session
			}
		}
		for j := range phase.Receipts {
			phase.Receipts[j].Identity.SessionID = session
			phase.Receipts[j].Result.ID = session
		}
		input.Phases = append(input.Phases, phase)
	}
	return input
}

func TestFreeRehearsalKeepsStrictJudgeTiming(t *testing.T) {
	input := freeRehearsalTimingInput("first content not proven throughout event-time interval; width=1ms")
	strict := pooledPreviewMedia(&input)
	if strict.Status != "inconclusive" || strict.InconclusiveMetrics != 3 || len(strict.UndecidedMetrics) != 3 {
		t.Fatal(strict)
	}
	r := evaluateFreeRehearsalTiming(t, strict)
	if r.Verdict != operator.Passed || strict.Status != "inconclusive" || len(r.TimingUndecided) != 3 {
		t.Fatal("free policy changed strict media or rejected timing", r.Failures, strict)
	}
}

func TestDistributedWindowRowsUseOnlyMatchingEventCauses(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		causes        []string
	}{
		{"undecided", "inconclusive", []string{"event-window-incomplete"}},
		{"passing-with-stale-causes", "pass", []string{"metric-undecided", "trial-count-insufficient"}},
		{"no-own-causes", "inconclusive", []string{"metric-undecided", "trial-count-insufficient"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := previewSample()
			r.Events = r.Events[:2]
			call := r.Processes[0].Calls[0].Call
			call.Moves = call.Moves[:2]
			call.Moves[0].Distributed.WindowOutcome = "inconclusive"
			call.Moves[0].Distributed.Causes = []string{"first content: duration=[1.997s,2.003s]; event interval width=6ms"}
			call.Moves[1].Distributed.WindowOutcome = tc.outcome
			if tc.name != "no-own-causes" {
				call.Moves[1].Distributed.Causes = []string{"event window evidence incomplete", "event window evidence incomplete"}
			}
			// The join must use event identity, not the move's position.
			call.Moves[0], call.Moves[1] = call.Moves[1], call.Moves[0]
			r.Judge()
			want := []undecidedMetric{
				{Event: 0, Metric: "distributed_window", Causes: []string{"duration-bounds-uncertain"}},
				{Event: 1, Metric: "distributed_window", Causes: tc.causes},
			}
			if got := undecidedRows(&r); !reflect.DeepEqual(got, want) {
				t.Fatalf("event causes leaked or pool causes lost: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestDistributedWindowRowsKeepPoolOnlyCauses(t *testing.T) {
	r := previewSample()
	call := r.Processes[0].Calls[0].Call
	call.Distributed.ClockError = "first_content not proven throughout event-time interval; width=1ms"
	call.Moves[0].Distributed.WindowOutcome = "inconclusive"
	// The pool independently emits the same p95 reason. Removing window
	// reasons by set membership would lose that pool-owned occurrence.
	call.Moves[0].Distributed.Causes = []string{"audio move p95 policy not proven across clock interval", "audio move p95 policy not proven across clock interval"}
	for i := 0; i < 20; i++ {
		call.Moves[i].Distributed.Gaps[0].EventGap.Upper = 200 * time.Millisecond
	}
	call.Moves[1].Distributed.Gaps[1].BaselineCertain = nil
	spread := make([]time.Duration, 50)
	for i := range spread {
		spread[i] = 20 * time.Millisecond
		if i >= 30 {
			spread[i] = 100 * time.Millisecond
		}
	}
	call.Moves[2].Distributed.Gaps[1].BaselineCertain = spread
	call.Moves[2].Distributed.Gaps[1].BaselinePossible = spread
	r.Judge()
	before, _ := json.Marshal(r)
	rows := undecidedRows(&r)
	if len(rows) != 30 || !reflect.DeepEqual(rows[0].Causes, []string{"move-p95-unproven"}) {
		t.Fatal(rows)
	}
	want := []string{"baseline-intervals-insufficient", "baseline-spread-uncertain", "classification-unproven-within-clock-interval", "metric-undecided", "move-p95-unproven"}
	for _, row := range rows[1:] {
		if !reflect.DeepEqual(row.Causes, want) {
			t.Fatalf("pool causes lost: got %+v, want %v", row, want)
		}
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("diagnostic projection changed the product report")
	}
}

func TestUndecidedRowsKeepCoverageFallbackWithoutReasons(t *testing.T) {
	r := previewSample()
	r.Processes[0].Calls[0].Call.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{}
	r.Judge()
	rows := undecidedRows(&r)
	if len(rows) != 1 || rows[0].Event != 0 || rows[0].Metric != "first_content" || !reflect.DeepEqual(rows[0].Causes, []string{"trusted-coverage-below-80-percent"}) {
		t.Fatal(rows)
	}
}

func evaluateFreeRehearsalTiming(t *testing.T, strict previewMediaSummary) operatorrehearsal.Report {
	t.Helper()
	raw, _ := json.Marshal(strict)
	pool, err := operatorevidence.DecodeSummary(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := operatorrehearsal.Report{ScenarioSHA256: strings.Repeat("a", 64), Cleanup: true, CPU: operatorrehearsal.CPUReport{Calibration: operatorrehearsal.Calibration{Passed: true}}, Pool: &pool}
	for _, gate := range operatorrehearsal.CoveredGates {
		r.Gates = append(r.Gates, operatorrehearsal.GateReport{Gate: gate, ProcedureCompleted: true, CallerStopped: true, ProcedureResult: operatorgates.ProcedureResult{Status: operator.Inconclusive, Process: operatorgates.ProcessEvidence{CompletedSteps: 1}, Media: &operatorgates.MediaSummary{Status: operator.Inconclusive}}})
	}
	zero, one := uint64(0), uint64(1)
	r.Gates[3].Process = operatorgates.ProcessEvidence{CompletedSteps: 7, Restarts: 1, DuplicatePackets: &zero, RelayEpochDelta: &one, RoutesRestored: &one, RelayLocalTiming: "inconclusive", RelayConnectionInvariants: "inconclusive"}
	r.Evaluate()
	return r
}

func TestUnknownOwnTimingCausesKeepRehearsalVerdict(t *testing.T) {
	for _, reason := range []string{"first decoded content identity unavailable", "mapped event history or settle window unavailable", "insufficient caller window coverage"} {
		t.Run(reason, func(t *testing.T) {
			input := freeRehearsalTimingInput(reason)
			for i := 0; i < 3; i++ {
				one := previewPoolInput{Phases: []previewMediaInput{input.Phases[i]}}
				replaceCall(t, &one, func(c *callharness.Report) {
					c.Moves[0].Distributed.WindowOutcome = "inconclusive"
					c.Moves[0].Distributed.Causes = []string{reason}
				})
				input.Phases[i] = one.Phases[0]
			}
			strict := pooledPreviewMedia(&input)
			if strict.Status != "inconclusive" || strict.InconclusiveMetrics != 93 || len(strict.UndecidedMetrics) != 93 {
				t.Fatal(strict)
			}
			for _, row := range strict.UndecidedMetrics {
				if row.Event == 0 && !reflect.DeepEqual(row.Causes, []string{"trusted-coverage-below-80-percent"}) {
					t.Fatal("unknown own reason changed its fallback", row)
				}
			}
			r := evaluateFreeRehearsalTiming(t, strict)
			if r.Verdict != operator.Passed || r.Pool.Status != operator.Inconclusive {
				t.Fatal("diagnostic reason changed rehearsal verdict", r.Failures)
			}
		})
	}
}
