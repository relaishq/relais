package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
)

// boundaryVerdictSample is the pinned caller's verdict for a takeover whose
// label differs across the kill-time interval; extra reasons come first, as in
// pkg/callharness TestFrameInFlightAtKillLeavesOnlyIntervalReasons.
func boundaryVerdictSample(metric string, extra ...string) callharness.MetricVerdict {
	return callharness.MetricVerdict{Reasons: append(extra, metric+" classification changes within event-time interval; width=6ms", "duration=[347ms,353ms]; event interval width=6ms")}
}

// boundaryMove gives one takeover the caller's whole undecided record: both
// content verdicts, the measurement summary and the inconclusive window.
func boundaryMove(m *callharness.MoveReport, extra ...string) {
	v := &m.Measurement
	v.FirstContentVerdict = boundaryVerdictSample("first_content")
	v.FirstNewContentVerdict = boundaryVerdictSample("first_new_content", extra...)
	v.Inconclusive, v.Reasons = true, nil
	for _, reason := range v.FirstContentVerdict.Reasons {
		v.Reasons = append(v.Reasons, "first content: "+reason)
	}
	for _, reason := range v.FirstNewContentVerdict.Reasons {
		v.Reasons = append(v.Reasons, "first new content: "+reason)
	}
	d := *m.Distributed
	d.Evaluations, d.WindowOutcome, d.Outcome = 66, "inconclusive", "inconclusive"
	d.Causes = append([]string{}, v.Reasons...)
	m.Distributed = &d
}

// boundaryRun judges the pool, then judges its sanitized archive, which keeps
// only fixed reason prefixes. Both must give the same summary.
func boundaryRun(t *testing.T, change func(*callharness.MoveReport)) previewMediaSummary {
	t.Helper()
	input := rawPoolSample()
	editSampleTakeover(t, &input, change)
	qualifiedPool(&input, nil)
	raw, _ := json.Marshal(input)
	var pool operatorevidence.Pool
	if err := json.Unmarshal(raw, &pool); err != nil {
		t.Fatal(err)
	}
	pool.ClockCoverage = nil
	clean, err := operatorrehearsal.SanitizeMediaEvidence(pool)
	if err != nil {
		t.Fatal(err)
	}
	sanitized, err := decodePool(clean)
	if err != nil {
		t.Fatal(err)
	}
	sanitized.ClockCoverage = input.ClockCoverage
	got := pooledPreviewMedia(&input)
	if again := pooledPreviewMedia(&sanitized); !reflect.DeepEqual(got, again) {
		t.Fatalf("sanitized evidence judged differently: got %+v want %+v", again, got)
	}
	return got
}

// A pool with no boundary takeover that passes keeps its exact summary bytes.
// The fixture was written before the boundary rule existed. Regenerate only
// deliberately: RELAIS_UPDATE_BOUNDARY_FIXTURE=1.
func TestNoBoundaryTakeoverKeepsSummaryBytes(t *testing.T) {
	scenarios := map[string]func(*testing.T) previewPoolInput{
		"undecided-move": func(t *testing.T) previewPoolInput {
			input := rawPoolSample()
			editPoolCalls(t, &input, func(c *callharness.Report) {
				if c.Moves[0].Kind != "move" {
					t.Fatal("fixture move changed")
				}
				boundaryMove(&c.Moves[0])
			})
			qualifiedPool(&input, nil)
			return input
		},
		"failing-kill-time": func(t *testing.T) previewPoolInput {
			input := rawPoolSample()
			editSampleTakeover(t, &input, func(m *callharness.MoveReport) {
				boundaryMove(m, "no decoded new content after resume before limit")
				m.Measurement.FirstContentVerdict = boundaryVerdictSample("first_content", "video baseline cannot distinguish live from outage media")
			})
			qualifiedPool(&input, nil)
			return input
		},
		"coverage-cause": func(t *testing.T) previewPoolInput {
			input := rawPoolSample()
			editSampleTakeover(t, &input, func(m *callharness.MoveReport) { boundaryMove(m) })
			qualifiedPool(&input, map[[2]int][]string{sampleTakeover: {"clock-coverage-bound"}})
			return input
		},
	}
	got := map[string]json.RawMessage{}
	for name, build := range scenarios {
		input := build(t)
		raw, err := json.Marshal(pooledPreviewMedia(&input))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := operatorevidence.DecodeSummary(raw)
		again, _ := json.Marshal(decoded)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("operator summary bytes differ", name, err)
		}
		got[name] = raw
	}
	fixture, _ := json.MarshalIndent(got, "", "  ")
	fixture = append(fixture, '\n')
	const name = "testdata/boundary-none.json"
	if os.Getenv("RELAIS_UPDATE_BOUNDARY_FIXTURE") == "1" {
		if err := os.WriteFile(name, fixture, 0600); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(fixture, frozen) {
		t.Fatal("MUTATION: boundary-rule-scope: summary bytes without a passing boundary takeover changed", err)
	}
}

var boundaryRowCauses = []string{"classification-varies-with-clock-interval", "duration-bounds-uncertain"}

// Owner decision #446, option C: a takeover whose label differs across the
// kill-time interval passes when every kill time passes. Its label stays in
// the summary.
func TestBoundaryTakeoverPassesWhenEveryKillTimePasses(t *testing.T) {
	got := boundaryRun(t, func(m *callharness.MoveReport) { boundaryMove(m) })
	if got.Status != "passed" || got.QualifiedTakeovers != 10 || got.InconclusiveMetrics != 0 || len(got.UndecidedMetrics) != 0 || got.EventCount != 30 {
		t.Fatalf("MUTATION: boundary-takeover-undecided: %+v", got)
	}
	want := []undecidedMetric{{1, 10, "first_content", boundaryRowCauses}, {1, 10, "first_new_content", boundaryRowCauses}}
	if !reflect.DeepEqual(got.BoundaryMetrics, want) {
		t.Fatalf("MUTATION: boundary-label-dropped: %+v", got.BoundaryMetrics)
	}
	// The operator summary keeps the same rows and bytes.
	raw, _ := json.Marshal(got)
	if !bytes.HasSuffix(raw, []byte(`"iceRestarts":0,"boundaryMetrics":[{"phase":1,"event":10,"metric":"first_content","causes":["classification-varies-with-clock-interval","duration-bounds-uncertain"]},{"phase":1,"event":10,"metric":"first_new_content","causes":["classification-varies-with-clock-interval","duration-bounds-uncertain"]}]}`)) {
		t.Fatal("summary bytes", string(raw))
	}
	decoded, err := operatorevidence.DecodeSummary(raw)
	again, _ := json.Marshal(decoded)
	if err != nil || len(decoded.BoundaryMetrics) != 2 || !bytes.Equal(raw, again) {
		t.Fatal("MUTATION: boundary-label-dropped", err, string(again))
	}
	malformed := bytes.Replace(raw, []byte(`"boundaryMetrics":[{"phase":1,`), []byte(`"boundaryMetrics":[{`), 1)
	if _, err := operatorevidence.DecodeSummary(malformed); err == nil {
		t.Fatal("malformed boundary rows accepted")
	}
}

// One kill time that fails or is untrusted keeps the takeover undecided, and
// the undecided row keeps its label.
func TestBoundaryTakeoverStaysUndecidedWhenAKillTimeDoesNotPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*callharness.MoveReport)
		rows   int
	}{
		// first_content still passes for every kill time and keeps its row.
		{"failing-kill-time", func(m *callharness.MoveReport) {
			boundaryMove(m, "no decoded new content after resume before limit")
		}, 1},
		{"untrusted-kill-time", func(m *callharness.MoveReport) {
			boundaryMove(m, "video baseline cannot distinguish live from outage media")
		}, 1},
		{"reason-after-interval", func(m *callharness.MoveReport) {
			boundaryMove(m)
			v := &m.Measurement.FirstNewContentVerdict
			v.Reasons = append(v.Reasons[:1], "online decode samples unavailable or dropped")
		}, 1},
		{"reason-after-duration", func(m *callharness.MoveReport) {
			boundaryMove(m)
			v := &m.Measurement.FirstNewContentVerdict
			v.Reasons = append(v.Reasons, "online decode samples unavailable or dropped")
		}, 1},
		{"evaluation-budget", func(m *callharness.MoveReport) {
			boundaryMove(m)
			m.Measurement.FirstNewContentVerdict.Reasons = []string{"interval evaluation unavailable or exceeds 4096 evaluations; width=6ms"}
		}, 1},
		// A verdict without a label has no boundary rule.
		{"first-live", func(m *callharness.MoveReport) {
			boundaryMove(m)
			m.Measurement.FirstLiveVerdict = boundaryVerdictSample("first_live")
		}, 2},
		// The verdicts pass, but the window has a cause of its own.
		{"window-cause", func(m *callharness.MoveReport) {
			boundaryMove(m)
			m.Distributed.Causes = append(m.Distributed.Causes, "server operation failed or outcome uncertain")
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := boundaryRun(t, tc.change)
			if got.Status != "inconclusive" || got.QualifiedTakeovers != 9 || len(got.BoundaryMetrics) != tc.rows {
				t.Fatalf("MUTATION: boundary-takeover-partial-pass: %s status=%s takeovers=%d rows=%+v", tc.name, got.Status, got.QualifiedTakeovers, got.BoundaryMetrics)
			}
			labelled := false
			for _, row := range got.UndecidedMetrics {
				if row.Phase == 1 && row.Event == 10 && slices.Contains(row.Causes, "classification-varies-with-clock-interval") {
					labelled = true
				}
			}
			if !labelled && tc.name != "evaluation-budget" {
				t.Fatalf("undecided label lost: %+v", got.UndecidedMetrics)
			}
		})
	}
}

// A failed window on a boundary takeover stays failed and does not qualify.
func TestBoundaryTakeoverKeepsFailure(t *testing.T) {
	got := boundaryRun(t, func(m *callharness.MoveReport) {
		boundaryMove(m)
		m.Distributed.WindowOutcome, m.Distributed.Outcome = "fail", "fail"
	})
	if got.Status != "failed" || got.QualifiedTakeovers != 9 {
		t.Fatal("MUTATION: boundary-takeover-hides-failure", got.Status, got.QualifiedTakeovers)
	}
}

// Clock coverage still removes trust from a boundary takeover.
func TestBoundaryTakeoverKeepsClockCoverage(t *testing.T) {
	input := rawPoolSample()
	editSampleTakeover(t, &input, func(m *callharness.MoveReport) { boundaryMove(m) })
	qualifiedPool(&input, map[[2]int][]string{sampleTakeover: {"clock-coverage-bound"}})
	got := pooledPreviewMedia(&input)
	if got.Status != "inconclusive" || len(got.BoundaryMetrics) != 0 || !slices.Contains(coverageCauses(got), "clock-coverage-bound") {
		t.Fatalf("coverage cause lost: %+v", got)
	}
}

// A kill moves every call on the worker. Their boundary verdicts share one row
// per metric.
func TestBoundaryRowsOncePerEventAndMetric(t *testing.T) {
	r := &loadgen.RunReport{Events: []loadgen.EventResult{{Event: loadgen.Event{Kind: "kill"}}}}
	process := loadgen.ProcessReport{}
	for _, session := range []string{"a", "b"} {
		id := callharness.OperationIdentity{RunID: "run", OperationID: "kill", SessionID: session, Worker: "w1", Epoch: 1}
		move := callharness.MoveReport{Kind: "takeover", Identity: &id, Distributed: &callharness.DistributedMeasurement{}}
		boundaryMove(&move)
		r.Events[0].Identities = append(r.Events[0].Identities, id)
		process.Calls = append(process.Calls, loadgen.Message{Call: &callharness.Report{Moves: []callharness.MoveReport{move}}})
	}
	r.Processes = []loadgen.ProcessReport{process}
	if rows := boundaryRows(r); len(rows) != 0 {
		t.Fatal("MUTATION: boundary-row-untrusted", rows)
	}
	settleBoundaryTakeovers(r)
	want := []undecidedMetric{{0, 0, "first_content", boundaryRowCauses}, {0, 0, "first_new_content", boundaryRowCauses}}
	if rows := boundaryRows(r); !reflect.DeepEqual(rows, want) {
		t.Fatalf("MUTATION: boundary-row-per-call: %+v", rows)
	}
}
