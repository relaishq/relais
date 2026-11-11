package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/callharness"
)

// A pool with no excluded takeover keeps its exact summary bytes. Regenerate
// only deliberately: RELAIS_UPDATE_EXCLUDED_FIXTURE=1.
func TestNoExcludedTakeoverKeepsSummaryBytes(t *testing.T) {
	scenarios := map[string]func() previewPoolInput{
		"clean": func() previewPoolInput {
			input := rawPoolSample()
			qualifiedPool(&input, nil)
			return input
		},
		"clean-replacement": func() previewPoolInput {
			input := replacementPoolSample()
			qualifiedPool(&input, nil)
			return input
		},
		"kill-bracket": func() previewPoolInput {
			input := replacementPoolSample()
			qualifiedPool(&input, map[[2]int][]string{{1, 5}: {"clock-coverage-kill-bracket"}})
			return input
		},
	}
	got := map[string]json.RawMessage{}
	for name, build := range scenarios {
		input := build()
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
	const name = "testdata/excluded-none.json"
	if os.Getenv("RELAIS_UPDATE_EXCLUDED_FIXTURE") == "1" {
		if err := os.WriteFile(name, fixture, 0600); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(fixture, frozen) {
		t.Fatal("summary bytes without an excluded takeover changed", err)
	}
}

// editExcludedMove edits the caller move of one pooled event.
func editExcludedMove(t *testing.T, input *previewPoolInput, at [2]int, change func(*callharness.MoveReport)) {
	t.Helper()
	id := input.Phases[at[0]].Events[at[1]].Identities[0]
	editPoolCalls(t, input, func(c *callharness.Report) {
		for i := range c.Moves {
			if c.Moves[i].Identity != nil && *c.Moves[i].Identity == id {
				change(&c.Moves[i])
			}
		}
	})
}

func excludedRun(t *testing.T, change func(*callharness.MoveReport)) previewMediaSummary {
	t.Helper()
	input := replacementPoolSample()
	if change != nil {
		editExcludedMove(t, &input, [2]int{1, 5}, change)
	}
	qualifiedPool(&input, map[[2]int][]string{{1, 5}: {"clock-coverage-kill-excluded"}})
	return pooledPreviewMedia(&input)
}

// The excluded takeover gets its own row from the counted-back-in judgment.
func TestExcludedTakeoverReportsItsRow(t *testing.T) {
	got := excludedRun(t, nil)
	e := got.Excluded
	if got.Status != "passed" || got.ExcludedKills != 1 || e == nil || e.CountedBackIn != "passed" || e.OwnerSignOff || len(e.Takeovers) != 1 {
		t.Fatalf("MUTATION: excluded-takeover-unreported: %+v %+v", got, e)
	}
	row := e.Takeovers[0]
	if row.Phase != 1 || row.Event != 5 || !row.NonTimingPassed || len(row.Calls) != 1 || len(row.Metrics) != 9 {
		t.Fatalf("excluded row: %+v", row)
	}
	call := row.Calls[0]
	if call.WindowOutcome != "pass" || len(call.EventGaps) != 2 || call.EventGaps[0].Kind != "audio" || call.EventGaps[1].Kind != "video" || call.EventGaps[0].UpperNS <= 0 {
		t.Fatalf("excluded call: %+v", call)
	}
	for _, m := range row.Metrics {
		if m.Outcome != "pass" {
			t.Fatal("clean excluded metric", m)
		}
	}
	// The operator summary decodes the same bytes and requires the rows.
	raw, _ := json.Marshal(got)
	if !bytes.Contains(raw, []byte(`"iceRestarts":0,"excluded":{"takeovers":[{"phase":1,"event":5,`)) || !bytes.HasSuffix(raw, []byte(`"countedBackIn":"passed","ownerSignOff":false}}`)) {
		t.Fatal("summary bytes", string(raw))
	}
	decoded, err := operatorevidence.DecodeSummary(raw)
	again, _ := json.Marshal(decoded)
	if err != nil || decoded.Excluded == nil || !bytes.Equal(raw, again) {
		t.Fatal("MUTATION: excluded-rows-optional", err, string(again))
	}
	without := bytes.Replace(raw, raw[bytes.Index(raw, []byte(`,"excluded":`)):len(raw)-1], nil, 1)
	if _, err := operatorevidence.DecodeSummary(without); err == nil {
		t.Fatal("MUTATION: excluded-rows-optional", string(without))
	}
}

// A strict pass needs the excluded takeover's non-timing checks to pass. An
// undecided loss check holds the pass for owner sign-off; an undecided timing
// check does not.
func TestExcludedTakeoverNonTimingChecks(t *testing.T) {
	undecided := callharness.MetricVerdict{Reasons: []string{"loss unproven"}}
	for _, tc := range []struct {
		name     string
		change   func(*callharness.MoveReport)
		status   string
		signOff  bool
		nonTimed bool
	}{
		{"audio-loss", func(m *callharness.MoveReport) { m.Measurement.AudioLoss = undecided }, "inconclusive", true, false},
		{"video-freshness", func(m *callharness.MoveReport) { m.Measurement.Video.Verdict = undecided }, "inconclusive", true, false},
		{"first-content", func(m *callharness.MoveReport) { m.Measurement.FirstContentVerdict = undecided }, "passed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := excludedRun(t, tc.change)
			e := got.Excluded
			if e == nil || len(e.Takeovers) != 1 {
				t.Fatalf("missing excluded row: %+v", got)
			}
			if got.Status != tc.status || e.OwnerSignOff != tc.signOff || e.Takeovers[0].NonTimingPassed != tc.nonTimed || e.CountedBackIn != "inconclusive" {
				t.Fatalf("MUTATION: excluded-takeover-strict-pass: %s status=%s %+v", tc.name, got.Status, e)
			}
			// The pooled rows still describe only the pooled events.
			if got.InconclusiveMetrics != 0 || len(got.UndecidedMetrics) != 0 || got.EventCount != 30 {
				t.Fatalf("pooled rows changed: %+v", got)
			}
		})
	}
}

// A short pool stays inconclusive and still reports its excluded takeover.
func TestExcludedTakeoverWithoutReplacementReportsRow(t *testing.T) {
	input := rawPoolSample()
	qualifiedPool(&input, map[[2]int][]string{sampleTakeover: {"clock-coverage-kill-excluded"}})
	got := pooledPreviewMedia(&input)
	e := got.Excluded
	if got.Status != "inconclusive" || e == nil || e.CountedBackIn != "passed" || e.OwnerSignOff || len(e.Takeovers) != 1 || e.Takeovers[0].Phase != 1 || e.Takeovers[0].Event != 10 {
		t.Fatalf("short pool excluded row: %+v %+v", got, e)
	}
}

// The judge rechecks the gate's exclusion from real timelines: 0.1 ms error on
// the worker and frontend plus the half-width must exceed the 3 ms bound. A
// 2.9 ms half-width alone fits but is still too wide with the clock error.
func TestExcludedTakeoverBracketRecheck(t *testing.T) {
	for _, tc := range []struct {
		name         string
		below, above int64
		excluded     bool
	}{
		{"fits", 2800000, 2800000, false},
		{"one-over", 2800000, 2800001, true},
		{"half-width-fits-alone", 2900000, 2900000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, causes := excludedBracketPool(t, tc.below, tc.above, nil)
			got := pooledPreviewMedia(&input)
			if got.Status != "passed" {
				t.Fatalf("status: %+v", got)
			}
			if !tc.excluded {
				if len(causes) != 0 || got.ExcludedKills != 0 || got.EventCount != 31 || got.Excluded != nil {
					t.Fatalf("MUTATION: excluded-bracket-trusts-gate: causes=%v %+v", causes, got)
				}
				return
			}
			if !slices.Equal(causes, []string{"clock-coverage-kill-excluded"}) || got.ExcludedKills != 1 || got.EventCount != 30 || got.Excluded == nil {
				t.Fatalf("wide bracket not excluded: causes=%v %+v", causes, got)
			}
		})
	}
}
