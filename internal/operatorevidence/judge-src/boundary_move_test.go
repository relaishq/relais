package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/callharness"
)

// rawPoolSample phase 0 event 0 is the planned move "event-0".
var sampleMove = [2]int{0, 0}

// boundaryMoveRun gives one planned move the caller's label-only record, then
// judges the pool raw and sanitized.
func boundaryMoveRun(t *testing.T, change func(*callharness.MoveReport)) previewMediaSummary {
	t.Helper()
	input := rawPoolSample()
	if input.Phases[sampleMove[0]].Events[sampleMove[1]].Event.Kind != "move" {
		t.Fatal("fixture move changed")
	}
	editExcludedMove(t, &input, sampleMove, change)
	qualifiedPool(&input, nil)
	return judgeRawAndSanitized(t, input)
}

// disagreeingInterpretation is the reason of one untrusted interpretation.
// Since 2026-11-14 a move verdict whose only reason is the interval reason
// passes, so a test that needs an undecided move verdict puts this reason first.
const disagreeingInterpretation = "video baseline cannot distinguish live from outage media"

// boundaryMoveDisagrees gives both content verdicts of a move the extra reason
// of one failing or untrusted interpretation (none when extra is empty).
func boundaryMoveDisagrees(m *callharness.MoveReport, extra string) {
	if extra == "" {
		boundaryMove(m)
		return
	}
	boundaryMove(m, extra)
	m.Measurement.FirstContentVerdict = boundaryVerdictSample("first_content", extra)
	v := &m.Measurement
	v.Reasons = nil
	for _, reason := range v.FirstContentVerdict.Reasons {
		v.Reasons = append(v.Reasons, "first content: "+reason)
	}
	for _, reason := range v.FirstNewContentVerdict.Reasons {
		v.Reasons = append(v.Reasons, "first new content: "+reason)
	}
	m.Distributed.Causes = append([]string{}, v.Reasons...)
}

// boundaryMoveDisagreements are moves whose verdict, not only its label,
// differs or stays unproven across the interval, or whose window has another
// cause. None of them is label-only.
var boundaryMoveDisagreements = map[string]func(*callharness.MoveReport){
	"failing-interpretation": func(m *callharness.MoveReport) {
		boundaryMove(m, "no decoded new content after resume before limit")
	},
	"untrusted-interpretation": func(m *callharness.MoveReport) {
		boundaryMove(m, "video baseline cannot distinguish live from outage media")
	},
	"reason-after-duration": func(m *callharness.MoveReport) {
		boundaryMove(m)
		v := &m.Measurement.FirstNewContentVerdict
		v.Reasons = append(v.Reasons, "online decode samples unavailable or dropped")
	},
	"evaluation-budget": func(m *callharness.MoveReport) {
		boundaryMove(m)
		m.Measurement.FirstNewContentVerdict.Reasons = []string{"interval evaluation unavailable or exceeds 4096 evaluations; width=6ms"}
	},
	"first-live": func(m *callharness.MoveReport) {
		boundaryMove(m)
		m.Measurement.FirstLiveVerdict = boundaryVerdictSample("first_live")
	},
	"window-cause": func(m *callharness.MoveReport) {
		boundaryMove(m)
		m.Distributed.Causes = append(m.Distributed.Causes, "video: [gap judgment varies within clock interval]")
	},
	"window-fail": func(m *callharness.MoveReport) {
		boundaryMove(m)
		m.Distributed.WindowOutcome, m.Distributed.Outcome = "fail", "fail"
	},
	"coverage-cause": func(m *callharness.MoveReport) { boundaryMove(m) },
}

var boundaryMoveRows = []undecidedMetric{{0, 0, "first_content", boundaryRowCauses}, {0, 0, "first_new_content", boundaryRowCauses}}

// Owner decision 2026-11-14 (option A): a move whose label differs across the
// event-time interval passes when every start and end time passes. Its label
// stays in the summary. Before the decision it stayed undecided and left 19
// qualified moves.
func TestBoundaryMovePassesWhenEveryInterpretationPasses(t *testing.T) {
	got := boundaryMoveRun(t, func(m *callharness.MoveReport) { boundaryMove(m) })
	if got.Status != "passed" || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 || got.InconclusiveMetrics != 0 || len(got.UndecidedMetrics) != 0 {
		t.Fatalf("MUTATION: boundary-move-undecided: %+v", got)
	}
	if !reflect.DeepEqual(got.BoundaryMetrics, boundaryMoveRows) {
		t.Fatalf("MUTATION: boundary-move-label-dropped: %+v", got.BoundaryMetrics)
	}
	raw, _ := json.Marshal(got)
	decoded, err := operatorevidence.DecodeSummary(raw)
	again, _ := json.Marshal(decoded)
	if err != nil || !reflect.DeepEqual(decoded.BoundaryMetrics, []operatorevidence.UndecidedMetric{{Phase: 0, Event: 0, Metric: "first_content", Causes: boundaryRowCauses}, {Phase: 0, Event: 0, Metric: "first_new_content", Causes: boundaryRowCauses}}) || !bytes.Equal(raw, again) {
		t.Fatal("MUTATION: boundary-move-label-dropped", err, string(again))
	}
}

// One start or end time that fails or is untrusted keeps the move undecided,
// as does a window cause of its own. A verdict that is still label-only keeps
// its row.
func TestBoundaryMoveStaysUndecidedWhenAnInterpretationDoesNotPass(t *testing.T) {
	rows := map[string]int{"failing-interpretation": 1, "untrusted-interpretation": 1, "reason-after-duration": 1, "evaluation-budget": 1, "first-live": 2, "window-cause": 2}
	for name, want := range rows {
		t.Run(name, func(t *testing.T) {
			got := boundaryMoveRun(t, boundaryMoveDisagreements[name])
			if got.Status != "inconclusive" || got.QualifiedMoves != 19 || len(got.BoundaryMetrics) != want {
				t.Fatalf("MUTATION: boundary-move-partial-pass: %s status=%s moves=%d rows=%+v", name, got.Status, got.QualifiedMoves, got.BoundaryMetrics)
			}
			labelled := false
			for _, row := range got.UndecidedMetrics {
				if row.Phase == 0 && row.Event == 0 && slices.Contains(row.Causes, "classification-varies-with-clock-interval") {
					labelled = true
				}
			}
			if !labelled {
				t.Fatalf("undecided label lost: %+v", got.UndecidedMetrics)
			}
		})
	}
}

// Both content verdicts fail or are untrusted for one interpretation: nothing
// is trusted and no row is listed.
func TestBoundaryMoveVerdictDisagreementStaysUndecided(t *testing.T) {
	for _, extra := range []string{"no decoded new content after resume before limit", "video baseline cannot distinguish live from outage media"} {
		got := boundaryMoveRun(t, func(m *callharness.MoveReport) { boundaryMoveDisagrees(m, extra) })
		if got.Status != "inconclusive" || got.QualifiedMoves != 19 || len(got.BoundaryMetrics) != 0 {
			t.Fatalf("MUTATION: boundary-move-disagreement-trusted: %s %+v", extra, got)
		}
	}
}

// A failed move window stays failed and does not qualify.
func TestBoundaryMoveKeepsFailure(t *testing.T) {
	got := boundaryMoveRun(t, boundaryMoveDisagreements["window-fail"])
	if got.Status != "failed" || got.QualifiedMoves != 19 {
		t.Fatal("MUTATION: boundary-move-hides-failure", got.Status, got.QualifiedMoves)
	}
}

// Clock coverage still removes trust from a boundary move.
func TestBoundaryMoveKeepsClockCoverage(t *testing.T) {
	input := rawPoolSample()
	editExcludedMove(t, &input, sampleMove, boundaryMoveDisagreements["coverage-cause"])
	qualifiedPool(&input, map[[2]int][]string{sampleMove: {"clock-coverage-bound"}})
	got := pooledPreviewMedia(&input)
	if got.Status != "inconclusive" || len(got.BoundaryMetrics) != 0 || !slices.Contains(coverageCauses(got), "clock-coverage-bound") {
		t.Fatalf("coverage cause lost: %+v", got)
	}
}

// A boundary move and a boundary takeover each list their rows once.
func TestBoundaryMoveAndTakeoverRowsOnce(t *testing.T) {
	input := rawPoolSample()
	editExcludedMove(t, &input, sampleMove, func(m *callharness.MoveReport) { boundaryMove(m) })
	editSampleTakeover(t, &input, func(m *callharness.MoveReport) { boundaryMove(m) })
	qualifiedPool(&input, nil)
	got := judgeRawAndSanitized(t, input)
	want := append(slices.Clone(boundaryMoveRows), undecidedMetric{1, 10, "first_content", boundaryRowCauses}, undecidedMetric{1, 10, "first_new_content", boundaryRowCauses})
	if got.Status != "passed" || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 || !reflect.DeepEqual(got.BoundaryMetrics, want) {
		t.Fatalf("MUTATION: boundary-rows-duplicated: %s %+v", got.Status, got.BoundaryMetrics)
	}
}

// The counted-back-in judgment of an excluded takeover settles the moves too.
// Only the excluded takeover's rows come from it, so a move row stays single.
func TestBoundaryMoveRowOnceWithExcludedTakeover(t *testing.T) {
	input := replacementPoolSample()
	if input.Phases[sampleMove[0]].Events[sampleMove[1]].Event.Kind != "move" {
		t.Fatal("fixture move changed")
	}
	editExcludedMove(t, &input, sampleMove, func(m *callharness.MoveReport) { boundaryMove(m) })
	qualifiedPool(&input, map[[2]int][]string{{1, 5}: {"clock-coverage-kill-excluded"}})
	got := judgeRawAndSanitized(t, input)
	if got.Status != "passed" || got.ExcludedKills != 1 || got.Excluded == nil || got.Excluded.CountedBackIn != "passed" || !reflect.DeepEqual(got.BoundaryMetrics, boundaryMoveRows) {
		t.Fatalf("MUTATION: boundary-move-row-twice: %s %+v", got.Status, got.BoundaryMetrics)
	}
}

// A move whose first decoded content passes as usual can still have a
// label-only first new content. That verdict alone settles the move, with one
// row.
func TestBoundaryMoveSettlesOneLabelOnlyVerdict(t *testing.T) {
	got := boundaryMoveRun(t, func(m *callharness.MoveReport) {
		boundaryMove(m)
		v := &m.Measurement
		v.FirstContentVerdict = callharness.MetricVerdict{Trusted: true}
		v.Reasons = nil
		for _, reason := range v.FirstNewContentVerdict.Reasons {
			v.Reasons = append(v.Reasons, "first new content: "+reason)
		}
		m.Distributed.Causes = append([]string{}, v.Reasons...)
	})
	if got.Status != "passed" || got.QualifiedMoves != 20 || got.InconclusiveMetrics != 0 || !reflect.DeepEqual(got.BoundaryMetrics, boundaryMoveRows[1:]) {
		t.Fatalf("MUTATION: boundary-move-single-verdict: %s moves=%d rows=%+v", got.Status, got.QualifiedMoves, got.BoundaryMetrics)
	}
}
