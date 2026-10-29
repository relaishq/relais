package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/callharness"
)

func relayInput() previewMediaInput {
	r := previewSample()
	c := r.Processes[0].Calls[0].Call
	c.Moves = nil
	c.Distributed.Armed = nil
	r.Events = []loadgen.EventResult{}
	input := rawPreviewInput(r, 0)
	input.Gate = "relay-deploy"
	input.Receipts = []previewReceipt{}
	return input
}
func TestRelayCompletedCallContributesNoTrials(t *testing.T) {
	input := relayInput()
	got := pooledPreviewMedia(&previewPoolInput{RunID: input.RunID, Phases: []previewMediaInput{input}})
	if got.Status != "passed" || got.ExpectedCalls != 1 || got.CompletedCalls != 1 || got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 || got.EventCount != 0 || got.MetricCount != 0 || got.RelayCalls != 1 || got.IncludedPhases != 1 || got.DroppedPhases != 0 {
		t.Fatal("relay completed call was excluded or counted as trials", got)
	}
}
func TestOtherEventlessGatesRemainExcluded(t *testing.T) {
	for _, gate := range []string{"", "rollout", "control-restart", "upgrade-rollback", "failover", "load", "unknown"} {
		t.Run(gate, func(t *testing.T) {
			input := relayInput()
			input.Gate = gate
			got := pooledPreviewMedia(&previewPoolInput{RunID: input.RunID, Phases: []previewMediaInput{input}})
			if got.Status == "passed" || got.IncludedPhases != 0 || got.DroppedPhases != 1 {
				t.Fatal(got)
			}
		})
	}
}
func TestRelayPoolKeepsTrialPopulationUnchanged(t *testing.T) {
	input := rawPoolSample()
	relay := relayInput()
	relay.Sessions = []string{"relay-session"}
	relay.CallerJSONL = strings.ReplaceAll(relay.CallerJSONL, `"session":"session"`, `"session":"relay-session"`)
	input.Phases = append(input.Phases, relay)
	got := pooledPreviewMedia(&input)
	if got.Status != "passed" || got.CompletedCalls != 3 || got.ExpectedCalls != 3 || got.QualifiedMoves != 20 || got.QualifiedTakeovers != 10 || got.EventCount != 30 || got.RelayCalls != 1 || got.IncludedPhases != 3 {
		t.Fatal(got)
	}
}
func TestFrozenRelayProcedureHasNoTrialReceipts(t *testing.T) {
	corpus, e := operatorgates.LoadCorpus("../../operatorgates/testdata/transcripts")
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"8f3705c63b25260b.json", "c84ac2a6e5929c83.json"} {
		t.Run(name, func(t *testing.T) {
			f, e := corpus.Fixture(name)
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, event := range f.Events {
				if strings.HasSuffix(event.Operation, ".broker.arm") || strings.HasSuffix(event.Operation, ".broker.feed") {
					t.Fatal("relay oracle unexpectedly emits trials", event.Operation)
				}
				if event.Kind != "result" || event.Operation != "processes.runProcessGate" {
					continue
				}
				var expected struct {
					Status  string
					Process struct{ RelayLocalTiming, RelayConnectionInvariants string }
					Media   struct {
						Status         string
						CompletedCalls int
					}
				}
				if e = json.Unmarshal(event.Value, &expected); e != nil {
					t.Fatal(e)
				}
				if expected.Status != "inconclusive" || expected.Process.RelayLocalTiming != "inconclusive" || expected.Process.RelayConnectionInvariants != "inconclusive" || expected.Media.Status != "passed" || expected.Media.CompletedCalls != 1 {
					t.Fatal(expected)
				}
				phase := relayInput()
				got := pooledPreviewMedia(&previewPoolInput{RunID: phase.RunID, Phases: []previewMediaInput{phase}})
				if got.Status != expected.Media.Status || got.CompletedCalls != expected.Media.CompletedCalls || got.EventCount != 0 || got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 {
					t.Fatal("relay media contract differs", got, expected)
				}
				found = true
			}
			if !found {
				t.Fatal("missing relay oracle result")
			}
		})
	}
}

func TestRelayStillRequiresCompleteCallerEvidence(t *testing.T) {
	for name, change := range map[string]func(*previewMediaInput){
		"unfinished": func(in *previewMediaInput) {
			in.CallerJSONL = strings.ReplaceAll(in.CallerJSONL, `"type":"done"`, `"type":"unfinished"`)
		},
		"missing-moves": func(in *previewMediaInput) { in.CallerJSONL = strings.Replace(in.CallerJSONL, `,"Moves":null`, "", 1) },
		"missing-armed": func(in *previewMediaInput) { in.CallerJSONL = strings.Replace(in.CallerJSONL, `,"armed":null`, "", 1) },
		"missing-stats": func(in *previewMediaInput) {
			in.CallerJSONL = strings.Replace(in.CallerJSONL, `"cpu_percent_per_gomaxprocs":0,`, "", 1)
		},
		"registration": func(in *previewMediaInput) { in.Probes = nil },
		"run":          func(in *previewMediaInput) { in.RunID = "other-run" },
		"trials":       func(in *previewMediaInput) { *in = rawPreviewInput(previewSample(), 0); in.Gate = "relay-deploy" },
		"generator-budget": func(in *previewMediaInput) {
			in.CallerJSONL = strings.ReplaceAll(in.CallerJSONL, `"cpu_percent_per_gomaxprocs":0`, `"cpu_percent_per_gomaxprocs":100`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := relayInput()
			original, _ := json.Marshal(in)
			change(&in)
			changed, _ := json.Marshal(in)
			if string(original) == string(changed) {
				t.Fatal("regression did not change input")
			}
			got := pooledPreviewMedia(&previewPoolInput{RunID: in.RunID, Phases: []previewMediaInput{in}})
			if got.Status == "passed" || got.IncludedPhases != 0 || got.DroppedPhases != 1 || got.EventCount != 0 || got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestRelayKnownFailureSurvivesMissingEvidence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		in := relayInput()
		pool := previewPoolInput{RunID: in.RunID, Phases: []previewMediaInput{in}}
		replaceCall(t, &pool, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
		if missing {
			pool.Phases[0].Probes = nil
		}
		got := pooledPreviewMedia(&pool)
		if got.Status != "failed" || got.EventCount != 0 || got.QualifiedMoves != 0 || got.QualifiedTakeovers != 0 {
			t.Fatal(got)
		}
	}
}

func TestIncompleteRelayCannotEraseTrialPoolFailure(t *testing.T) {
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		for i := range c.Moves {
			for j := range c.Moves[i].Distributed.Gaps {
				c.Moves[i].Distributed.Gaps[j].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
			}
		}
	})
	if !poolFailure(&input) {
		t.Fatal("missing failing trial control")
	}
	relay := relayInput()
	relay.Probes = nil
	input.Phases = append(input.Phases, relay)
	if !poolFailure(&input) {
		t.Fatal("an eventless relay entered the trial failure population")
	}
}

func TestRelayCoverageInventory(t *testing.T) {
	raw, err := os.ReadFile("../../operatorgates/testdata/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Fixtures []struct {
			Fixture            string
			MediaQualification struct {
				Evidence, Test, Scope                   string
				CompletedCalls, Events, Trials, Metrics *int
			}
		}
	}
	if err = json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{"8f3705c63b25260b.json": true, "c84ac2a6e5929c83.json": true}
	for _, row := range inventory.Fixtures {
		if !expected[row.Fixture] {
			continue
		}
		q := row.MediaQualification
		if q.Evidence != "tested" || q.Test != "TestFrozenRelayProcedureHasNoTrialReceipts" || q.Scope == "" || q.CompletedCalls == nil || *q.CompletedCalls != 1 || q.Events == nil || *q.Events != 0 || q.Trials == nil || *q.Trials != 0 || q.Metrics == nil || *q.Metrics != 0 {
			t.Fatal("relay zero-trial qualification is not inventoried", row.Fixture)
		}
		delete(expected, row.Fixture)
	}
	if len(expected) != 0 {
		t.Fatal("missing frozen relay inventory", expected)
	}
}
