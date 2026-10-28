package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/operator"
)

func timingReport() Report {
	r := passingReport()
	r.Pool.Status = operator.Inconclusive
	r.Pool.QualifiedMoves = 18 // Strict timing qualification is still reported.
	r.Pool.QualifiedTakeovers = 9
	r.Pool.MetricCount = 270
	r.Pool.InconclusiveMetrics = 3
	for i, metric := range []string{"first_content", "first_new_content", "distributed_window"} {
		r.Pool.UndecidedMetrics = append(r.Pool.UndecidedMetrics, operatorevidence.UndecidedMetric{Phase: i, Event: 0, Metric: metric, Causes: []string{"classification-varies-with-clock-interval"}})
		r.Gates[i].Status = operator.Inconclusive
		r.Gates[i].Media.Status = operator.Inconclusive
	}
	return r
}
func TestRehearsalAllowsUndecidedTiming(t *testing.T) {
	r := timingReport()
	r.Evaluate()
	if r.Verdict != operator.Passed {
		t.Fatal("MUTATION: rehearsal-timing-blocks", r.Failures)
	}
}

func TestRehearsalTimingDoesNotHideFailures(t *testing.T) {
	for name, mutate := range map[string]func(*Report){
		"failed-metric":        func(r *Report) { r.Pool.FailedMetrics = 1 },
		"dropped-phase":        func(r *Report) { r.Pool.DroppedPhases = 1 },
		"lost-call":            func(r *Report) { r.Pool.LostCalls = 1 },
		"decryption":           func(r *Report) { r.Pool.DecryptionFailures = 1 },
		"ice-restart":          func(r *Report) { r.Pool.ICERestarts = 1 },
		"excluded-event":       func(r *Report) { r.Pool.ExcludedEvents = 1 },
		"invalid-phase":        func(r *Report) { r.Pool.PhaseDiagnostics[0].Cause = "invalid-phase-evidence" },
		"missing-phase":        func(r *Report) { r.Pool.PhaseDiagnostics = r.Pool.PhaseDiagnostics[:3] },
		"missing-timing-row":   func(r *Report) { r.Pool.UndecidedMetrics = r.Pool.UndecidedMetrics[:2] },
		"duplicate-timing-row": func(r *Report) { r.Pool.UndecidedMetrics[1] = r.Pool.UndecidedMetrics[0] },
		"unallowed-metric":     func(r *Report) { r.Pool.UndecidedMetrics[0].Metric = "audio_loss" },
		"wrong-event":          func(r *Report) { r.Pool.UndecidedMetrics[0].Event = 99 },
		"invalid-cause":        func(r *Report) { r.Pool.UndecidedMetrics[0].Causes = []string{"unclassified"} },
		"missing-cause":        func(r *Report) { r.Pool.UndecidedMetrics[0].Causes = nil },
		"incomplete-media":     func(r *Report) { r.Pool.CompleteMedia = false },
		"incomplete-procedure": func(r *Report) { r.Gates[0].ProcedureCompleted = false },
		"zero-steps":           func(r *Report) { r.Gates[0].Process.CompletedSteps = 0 },
		"unknown":              func(r *Report) { r.Gates[0].Reason = "command-result-unknown" },
		"latch":                func(r *Report) { r.Latched = true },
		"uncertainty":          func(r *Report) { r.Uncertainties = []operator.Uncertainty{{Rule: "timeout"}} },
		"precondition":         func(r *Report) { r.Gates[0].Reason = "precondition-failed" },
		"driver":               func(r *Report) { r.Errors = []string{"sequence-failed"} },
		"interrupt":            func(r *Report) { r.Errors = []string{"interrupted"} },
		"cleanup":              func(r *Report) { r.Cleanup = false },
		"calibration":          func(r *Report) { r.CPU.Calibration.Passed = false },
	} {
		t.Run(name, func(t *testing.T) {
			r := timingReport()
			mutate(&r)
			r.Evaluate()
			if r.Verdict != operator.Failure {
				t.Fatal("MUTATION: rehearsal-timing-safety", name)
			}
		})
	}
}

func TestRehearsalReportsEveryUndecidedTiming(t *testing.T) {
	r := timingReport()
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Verdict != operator.Passed || saved.Pool.Status != operator.Inconclusive || len(saved.TimingUndecided) != 3 || !slices.Contains(saved.Limitations, "on a shared runner, media timing is judged strictly only in the paid run") {
		t.Fatal("timing report incomplete", out.String())
	}
	for i, row := range saved.TimingUndecided {
		if row.Phase != i || row.Gate != CoveredGates[i] || row.Event != 0 || len(row.Causes) != 1 || row.Causes[0] != "classification-varies-with-clock-interval" {
			t.Fatal(row)
		}
	}
	secret := "BEARER-private-value"
	r.Pool.UndecidedMetrics[0].Metric = secret
	r.Pool.UndecidedMetrics[1].Causes = []string{secret}
	r.Pool.PhaseDiagnostics[0].Gate = secret
	r.TimingUndecided = []TimingUndecided{{Gate: operator.Gate(secret)}}
	out.Reset()
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("timing diagnostics disclosed free text")
	}
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil || saved.Verdict != operator.Failure {
		t.Fatal("invalid timing evidence passed", err)
	}
}
