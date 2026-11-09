package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// Synthetic tracking data is explicit test input, never an upgrade of archives.
func coverageEvidenceFixture(t *testing.T) *clocks.CoverageEvidence {
	t.Helper()
	raw, _ := json.Marshal(rawPoolSample())
	var pool operatorevidence.Pool
	if err := json.Unmarshal(raw, &pool); err != nil {
		t.Fatal(err)
	}
	windows := operatorevidence.CoverageWindows(pool)
	e := &clocks.CoverageEvidence{Version: 2, FrontendSlot: "relay", ControlSlot: "shared", Bounds: clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}, Windows: windows}
	timelines := map[string]observe.CoverageTimeline{}
	start, _ := observe.CoverageNS(windows[0].StartUnixNS)
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		timeline := observe.CoverageTimeline{ReadComplete: true}
		for sec := start/1e9 - 5; sec <= start/1e9+100; sec++ {
			timeline.Rows = append(timeline.Rows, observe.CoverageRow{UnixSeconds: sec, MaxErrorSeconds: .0001, LeapStatus: "N"})
		}
		timelines[slot] = timeline
	}
	e.Attempts = []clocks.CoverageAttempt{{Version: 2, Clocks: timelines}}
	for _, w := range windows {
		if w.Kind == "takeover" {
			n, _ := observe.CoverageNS(w.StartUnixNS)
			e.KillBrackets = append(e.KillBrackets, observe.CoverageBracket{Worker: w.Worker, MidpointUnixNS: w.StartUnixNS, BeforeUnixNS: clocks.TimestampNS(n - 1), AfterUnixNS: clocks.TimestampNS(n + 1)})
		}
	}
	return e
}
func coveragePoolFixture(t *testing.T, scenario string) previewPoolInput {
	t.Helper()
	input := rawPoolSample()
	input.PreCoverageEvidence = false
	e := coverageEvidenceFixture(t)
	if scenario == "repair-gap-with-later-sample" {
		timeline := e.Attempts[0].Clocks["shared"]
		n, _ := observe.CoverageNS(e.Windows[0].StartUnixNS)
		timeline.Breaks = []observe.CoverageBreak{{FromUnixSeconds: n / 1e9, UnixSeconds: n / 1e9, Kind: "cursor-gap"}}
		e.Attempts[0].Clocks["shared"] = timeline
		// A later complete clean read repeats all rows, but cannot repair the loss.
		later := timeline
		later.Breaks = nil
		e.DrainAttempts = []clocks.CoverageAttempt{{Version: 2, Clocks: map[string]observe.CoverageTimeline{"shared": later}}}
	}
	if scenario == "omit-frontend-error" {
		e.Attempts[0].Clocks["relay"].Rows[6].MaxErrorSeconds = .002
	}
	if scenario == "omit-final-coverage" {
		timeline := e.Attempts[0].Clocks["shared"]
		n, _ := observe.CoverageNS(e.Windows[len(e.Windows)-1].EndUnixNS)
		for len(timeline.Rows) > 0 && timeline.Rows[len(timeline.Rows)-1].UnixSeconds*1e9 >= n {
			timeline.Rows = timeline.Rows[:len(timeline.Rows)-1]
		}
		e.Attempts[0].Clocks["shared"] = timeline
	}
	if scenario == "lease-missing" {
		n, _ := observe.CoverageNS(e.Windows[0].StartUnixNS)
		e.MissingIntervals = map[string]observe.CoverageBreak{"shared": {FromUnixSeconds: n / 1e9, UnixSeconds: n / 1e9, Kind: "clock-lease-expired"}}
	}
	q := e.Qualification()
	input.ClockCoverage = &q
	switch scenario {
	case "covered", "lease-missing", "repair-gap-with-later-sample", "omit-frontend-error", "omit-final-coverage":
	case "missing":
		input.ClockCoverage = nil
	case "falsified":
		q.Falsified = true
	case "independent-gap-failure":
		input.ClockCoverage = nil
		editPoolCalls(t, &input, func(c *callharness.Report) {
			c.Moves[0].Distributed.Outcome = "fail"
			c.Moves[0].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 2 * time.Second, Upper: 2 * time.Second}
		})
	case "independent-failure":
		input.ClockCoverage = nil
		editPoolCalls(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
	default:
		q.Results[0].Causes = []string{"clock-coverage-" + scenario}
	}
	return input
}

func TestClockCoverageQualifiesPhasesAndPool(t *testing.T) {
	for _, scenario := range []string{"covered", "missing", "lease-missing", "bound", "break", "kill-bracket", "falsified", "independent-failure", "independent-gap-failure", "repair-gap-with-later-sample", "omit-frontend-error", "omit-final-coverage"} {
		t.Run(scenario, func(t *testing.T) {
			input := coveragePoolFixture(t, scenario)
			got := pooledPreviewMedia(&input)
			want := "inconclusive"
			if scenario == "covered" {
				want = "passed"
			}
			if scenario == "independent-failure" || scenario == "independent-gap-failure" {
				want = "failed"
			}
			if got.Status != want || got.IncludedPhases != 2 || got.EventCount != 30 || got.MetricCount != 270 {
				t.Fatal("MUTATION: clock-coverage-qualification", scenario, got)
			}
			if scenario == "missing" || scenario == "falsified" {
				if got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 || got.InconclusiveMetrics != 270 {
					t.Fatal("MUTATION: clock-coverage-qualification", got)
				}
			}
			if scenario != "covered" && scenario != "independent-failure" && scenario != "independent-gap-failure" {
				wantCause := "clock-coverage-" + scenario
				if scenario == "lease-missing" || scenario == "repair-gap-with-later-sample" || scenario == "omit-final-coverage" {
					wantCause = "clock-coverage-missing"
				}
				if scenario == "omit-frontend-error" {
					wantCause = "clock-coverage-bound"
				}
				found := false
				for _, row := range got.UndecidedMetrics {
					found = found || slices.Contains(row.Causes, wantCause)
					if (scenario == "missing" || scenario == "falsified") && !slices.Contains(row.Causes, wantCause) {
						t.Fatal("metric lost clock cause", row)
					}
				}
				if !found {
					t.Fatal("MUTATION: clock-coverage-causes", wantCause, got.UndecidedMetrics)
				}
			}
			// Both phase invocations receive their own indexes, even when neither has
			// enough trials to pass independently.
			for i := range input.Phases {
				phase := &input.Phases[i]
				phase.coverage = coverageForPhase(&input, i)
				r, _, _ := parsePreviewMedia(phase)
				if scenario == "missing" || scenario == "falsified" {
					for _, p := range r.Processes {
						for _, c := range p.Calls {
							for _, m := range c.Call.Moves {
								if m.Distributed.WindowOutcome != "inconclusive" || m.Measurement.AudioLoss.Trusted || !m.Measurement.Inconclusive {
									t.Fatal("phase qualification missing", i, m)
								}
							}
						}
					}
				}
			}
		})
	}
}
func TestPreCoverageEvidenceIsExplicit(t *testing.T) {
	input := rawPoolSample()
	historical := pooledPreviewMedia(&input)
	input.PreCoverageEvidence = false
	current := pooledPreviewMedia(&input)
	if historical.Status != "passed" || current.Status != "inconclusive" {
		t.Fatal("MUTATION: explicit-pre-coverage", historical, current)
	}
	input.PreCoverageEvidence = true
	q := observe.CoverageQualification{Results: []observe.CoverageResult{}, Falsified: true}
	input.ClockCoverage = &q
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal("historical opt-in bypassed supplied coverage", got)
	}
}
func TestClockCoverageRetainsIndependentMetricFailure(t *testing.T) {
	r := previewSample()
	v := &r.Processes[0].Calls[0].Call.Moves[0].Measurement
	v.AudioLoss = callharness.MetricVerdict{Trusted: true, Failed: true}
	before := len(r.Processes[0].Calls[0].Call.Moves[0].Distributed.Gaps)
	got := previewMediaQualified(&r, false, map[int][]string{0: {"clock-coverage-missing"}})
	if got.Status != "failed" || got.FailedMetrics == 0 || !v.AudioLoss.Trusted || !v.Failed || !v.Inconclusive || before != len(r.Processes[0].Calls[0].Call.Moves[0].Distributed.Gaps) {
		t.Fatal("independent failure or populations lost", got, v)
	}
}
func TestClockCoverageTransportAndFrozenFixture(t *testing.T) {
	input := rawPoolSample()
	raw, _ := json.Marshal(input)
	var pool operatorevidence.Pool
	if err := json.Unmarshal(raw, &pool); err != nil {
		t.Fatal(err)
	}
	pool.PreCoverageEvidence = false
	pool.ClockCoverage = coverageEvidenceFixture(t)
	wire, err := operatorevidence.MarshalJudgeInput(pool)
	if err != nil || bytes.Contains(wire, []byte("maxErrorSeconds")) || len(wire) > len(raw)+4096 {
		t.Fatal("coverage transport budget", len(raw), len(wire), err)
	}
	decoded, err := decodePool(wire)
	if err != nil || pooledPreviewMedia(&decoded).Status != "passed" {
		t.Fatal("actual transport", err)
	}
	q := pool.ClockCoverage.Qualification()
	fixture, _ := json.MarshalIndent(q, "", "  ")
	fixture = append(fixture, '\n')
	const name = "testdata/clock-coverage.json"
	if os.Getenv("RELAIS_UPDATE_COVERAGE_FIXTURE") == "1" {
		if err := os.WriteFile(name, fixture, 0600); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(fixture, frozen) {
		t.Fatal("coverage-bearing fixture changed; regenerate deliberately", err)
	}
}

func TestMalformedClockQualificationCannotPass(t *testing.T) {
	for _, mode := range []string{"missing-results", "missing-causes", "null-causes", "duplicate-window"} {
		t.Run(mode, func(t *testing.T) {
			input := coveragePoolFixture(t, "covered")
			input.PreCoverageEvidence = true // Supplied bad evidence cannot use the archive path.
			raw, _ := json.Marshal(input)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			q := map[string]any{"results": []any{}, "falsified": false}
			switch mode {
			case "missing-results":
				delete(q, "results")
			case "missing-causes":
				q["results"] = []any{map[string]any{"phase": 0, "event": 0}}
			case "null-causes":
				q["results"] = []any{map[string]any{"phase": 0, "event": 0, "causes": nil}}
			case "duplicate-window":
				q["results"] = []any{map[string]any{"phase": 0, "event": 0, "causes": []string{}}, map[string]any{"phase": 0, "event": 0, "causes": []string{"clock-coverage-bound"}}}
			}
			object["clockCoverage"], _ = json.Marshal(q)
			wire, _ := json.Marshal(object)
			decoded, err := decodePool(wire)
			if err != nil {
				t.Fatal(err)
			}
			if got := pooledPreviewMedia(&decoded); got.Status != "inconclusive" || got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 {
				t.Fatal("malformed qualification restored trust", got)
			}
			decoded.Phases[0].Receipts[0].Result.Lost = true
			if got := pooledPreviewMedia(&decoded); got.Status != "failed" {
				t.Fatal("malformed qualification hid independent failure", got)
			}
		})
	}
}

func TestClockCoverageKeepsGapFailureButUntrustsWindow(t *testing.T) {
	r := previewSample()
	d := r.Processes[0].Calls[0].Call.Moves[0].Distributed
	d.Outcome = "fail" // An independent gap failure can coexist with a passing window.
	d.Gaps[0].EventGap = callharness.DurationInterval{Lower: 2 * time.Second, Upper: 2 * time.Second}
	got := previewMediaQualified(&r, false, map[int][]string{0: {"clock-coverage-bound"}})
	if got.Status != "failed" || d.WindowOutcome != "inconclusive" || d.Outcome != "fail" || r.Distributed.Outcome != "fail" {
		t.Fatal("independent gap failure hid clock uncertainty", got, d)
	}
}

func TestHistoricalCoverageAbsenceRequiresExplicitMarker(t *testing.T) {
	for _, historical := range []bool{false, true} {
		for _, coverage := range []string{"absent", "null", "{}"} {
			input := rawPoolSample()
			input.PreCoverageEvidence = historical
			raw, _ := json.Marshal(input)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if coverage != "absent" {
				object["clockCoverage"] = json.RawMessage(coverage)
			}
			raw, _ = json.Marshal(object)
			decoded, err := decodePool(raw)
			if err != nil {
				t.Fatal(err)
			}
			got := pooledPreviewMedia(&decoded)
			want := "inconclusive"
			if historical && coverage != "{}" {
				want = "passed"
			}
			if got.Status != want {
				t.Fatal("historical absence semantics", historical, coverage, got.Status, want)
			}
		}
	}
}
