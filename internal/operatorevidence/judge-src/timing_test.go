package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

func TestFreeRehearsalKeepsStrictJudgeTiming(t *testing.T) {
	input := previewPoolInput{RunID: "run-fixture"}
	for i, gate := range operatorrehearsal.CoveredGates {
		phase := relayInput()
		if i < 3 {
			r := previewSample()
			r.Processes[0].Calls[0].Call.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{"first content not proven throughout event-time interval; width=1ms"}}
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
	strict := pooledPreviewMedia(&input)
	if strict.Status != "inconclusive" || strict.InconclusiveMetrics != 3 || len(strict.UndecidedMetrics) != 3 {
		t.Fatal(strict)
	}
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
	if r.Verdict != operator.Passed || pool.Status != operator.Inconclusive || len(r.TimingUndecided) != 3 {
		t.Fatal("free policy changed strict media or rejected timing", r.Failures, pool)
	}
}
