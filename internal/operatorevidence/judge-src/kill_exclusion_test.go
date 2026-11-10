package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// qualifiedPool supplies one clean coverage row per event, then the given causes.
func qualifiedPool(input *previewPoolInput, causes map[[2]int][]string) {
	input.PreCoverageEvidence = false
	q := observe.CoverageQualification{Results: []observe.CoverageResult{}}
	for phase := range input.Phases {
		for event := range input.Phases[phase].Events {
			row := observe.CoverageResult{Phase: phase, Event: event, Causes: []string{}}
			if c, ok := causes[[2]int{phase, event}]; ok {
				row.Causes = c
			}
			q.Results = append(q.Results, row)
		}
	}
	input.ClockCoverage = &q
}

// rawPoolSample phase 1 event 10 is the takeover "event-25".
var sampleTakeover = [2]int{1, 10}

func editSampleTakeover(t *testing.T, input *previewPoolInput, change func(*callharness.MoveReport)) {
	t.Helper()
	editPoolCalls(t, input, func(c *callharness.Report) {
		for i := range c.Moves {
			if c.Moves[i].Identity.OperationID == "event-25" {
				change(&c.Moves[i])
			}
		}
	})
}

// A takeover whose timing is not trusted still fails the pool on independent
// loss or window failure. This holds for the kill-bracket cause today.
func TestUntrustedTakeoverKeepsIndependentFailure(t *testing.T) {
	for _, cause := range []string{"clock-coverage-kill-bracket", "clock-coverage-kill-excluded"} {
		for name, change := range map[string]func(*testing.T, *previewPoolInput){
			"lost-receipt": func(_ *testing.T, in *previewPoolInput) { in.Phases[1].Receipts[10].Result.Lost = true },
			"lost-move": func(t *testing.T, in *previewPoolInput) {
				editSampleTakeover(t, in, func(m *callharness.MoveReport) { m.Lost = true })
			},
			"window-fail": func(t *testing.T, in *previewPoolInput) {
				editSampleTakeover(t, in, func(m *callharness.MoveReport) {
					m.Distributed.Outcome = "fail"
					m.Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 2 * time.Second, Upper: 2 * time.Second}
				})
			},
		} {
			t.Run(cause+"/"+name, func(t *testing.T) {
				input := rawPoolSample()
				if input.Phases[1].Events[10].Event.Kind != "kill" {
					t.Fatal("fixture takeover moved")
				}
				change(t, &input)
				qualifiedPool(&input, map[[2]int][]string{sampleTakeover: {cause}})
				if got := pooledPreviewMedia(&input); got.Status != "failed" {
					t.Fatal("MUTATION: excluded-kill-hides-failure", cause, name, got.Status)
				}
			})
		}
	}
}

// withReplacement appends a 31st event: one more takeover after the 30 sample events.
func withReplacement() loadgen.RunReport {
	r := previewSample()
	call := r.Processes[0].Calls[0].Call
	m := call.Moves[29]
	id := *m.Identity
	id.OperationID, id.Epoch = "event-30", 31
	m.Identity = &id
	d := *m.Distributed
	m.Distributed = &d
	m.Start = 100 * time.Second
	m.End = m.Start + time.Millisecond
	d.Start = callharness.TimeInterval{Earliest: call.StartedAt.Add(m.Start), Latest: call.StartedAt.Add(m.Start)}
	d.End = callharness.TimeInterval{Earliest: call.StartedAt.Add(m.End), Latest: call.StartedAt.Add(m.End)}
	call.Moves = append(call.Moves, m)
	call.Distributed.Armed = append(call.Distributed.Armed, id)
	r.Events = append(r.Events, loadgen.EventResult{Event: loadgen.Event{Kind: "kill"}, Affected: 1, Sessions: []string{"session"}, Identities: []callharness.OperationIdentity{id}})
	return r
}

// replacementPoolSample splits 20 moves and 11 takeovers into two phases, like rawPoolSample.
func replacementPoolSample() previewPoolInput {
	a, b := withReplacement(), withReplacement()
	ca, cb := a.Processes[0].Calls[0].Call, b.Processes[0].Calls[0].Call
	ca.Moves, a.Events, ca.Distributed.Armed = ca.Moves[:15], a.Events[:15], ca.Distributed.Armed[:15]
	cb.Moves, b.Events, cb.Distributed.Armed = cb.Moves[15:], b.Events[15:], cb.Distributed.Armed[15:]
	b.Processes[0].Calls[0].Session = "second"
	for i := range cb.Moves {
		cb.Moves[i].Identity.SessionID = "second"
		cb.Distributed.Armed[i].SessionID = "second"
		b.Events[i].Sessions = []string{"second"}
		b.Events[i].Identities[0].SessionID = "second"
	}
	left, right := rawPreviewInput(a, 0), rawPreviewInput(b, 0)
	right.Sessions = []string{"second"}
	return previewPoolInput{RunID: "run-fixture", Phases: []previewMediaInput{left, right}}
}

func coverageCauses(got previewMediaSummary) []string {
	var causes []string
	for _, row := range got.UndecidedMetrics {
		for _, c := range row.Causes {
			if observe.IsCoverageCause(c) && !slices.Contains(causes, c) {
				causes = append(causes, c)
			}
		}
	}
	return causes
}

// One excluded takeover plus a passing replacement decides the pool normally.
func TestExcludedKillWithReplacementDecidesPool(t *testing.T) {
	for _, excluded := range []int{5, 15} {
		t.Run(fmt.Sprint(excluded), func(t *testing.T) {
			input := replacementPoolSample()
			if input.Phases[1].Events[excluded].Event.Kind != "kill" || len(input.Phases[1].Events) != 16 {
				t.Fatal("fixture takeover moved")
			}
			qualifiedPool(&input, map[[2]int][]string{{1, excluded}: {"clock-coverage-kill-excluded"}})
			got := pooledPreviewMedia(&input)
			if got.Status != "passed" || got.ExcludedKills != 1 || got.EventCount != 30 || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 || len(got.UndecidedMetrics) != 0 || got.ExcludedEvents != 0 {
				t.Fatalf("MUTATION: excluded-kill-not-dropped: %+v", got)
			}
			if got.PhaseDiagnostics[1].EventCount != 16 {
				t.Fatal("phase event count must keep the excluded kill", got.PhaseDiagnostics)
			}
		})
	}
}

// Without a replacement the pool is short, so it stays inconclusive. The
// remaining undecided rows keep their original phase and event positions.
func TestExcludedKillWithoutReplacementStaysInconclusive(t *testing.T) {
	input := rawPoolSample()
	qualifiedPool(&input, map[[2]int][]string{sampleTakeover: {"clock-coverage-kill-excluded"}})
	got := pooledPreviewMedia(&input)
	if got.Status != "inconclusive" || got.ExcludedKills != 1 || got.EventCount != 29 || len(coverageCauses(got)) != 0 {
		t.Fatalf("MUTATION: excluded-kill-not-dropped: %+v", got)
	}
	seen := map[[2]int]bool{}
	for _, row := range got.UndecidedMetrics {
		if row.Metric != "distributed_window" || !slices.Contains(row.Causes, "trial-count-insufficient") {
			t.Fatal("short pool row", row)
		}
		seen[[2]int{row.Phase, row.Event}] = true
	}
	if len(seen) != 29 || seen[sampleTakeover] || !seen[[2]int{1, 11}] || !seen[[2]int{1, 9}] || !seen[[2]int{0, 14}] {
		t.Fatal("undecided rows lost their event positions", seen)
	}
}

// Only an event whose sole clock cause is the excluded bracket leaves the pool.
func TestExcludedKillNeedsSoleCause(t *testing.T) {
	for name, causes := range map[string][]string{
		"kill-bracket": {"clock-coverage-kill-bracket"},
		"with-missing": {"clock-coverage-kill-excluded", "clock-coverage-missing"},
	} {
		t.Run(name, func(t *testing.T) {
			input := replacementPoolSample()
			qualifiedPool(&input, map[[2]int][]string{{1, 5}: causes})
			got := pooledPreviewMedia(&input)
			if got.Status != "inconclusive" || got.ExcludedKills != 0 || got.EventCount != 31 || len(coverageCauses(got)) == 0 {
				t.Fatalf("event with another clock cause left the pool: %+v", got)
			}
		})
	}
	input := replacementPoolSample()
	qualifiedPool(&input, map[[2]int][]string{{1, 5}: {"clock-coverage-kill-excluded"}})
	input.ClockCoverage.Falsified = true
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" || got.ExcludedKills != 0 {
		t.Fatalf("falsified coverage excluded a kill: %+v", got)
	}
}
