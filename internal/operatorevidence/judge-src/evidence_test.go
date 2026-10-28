package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestSanitizedArtifactRejudgesRawPhases(t *testing.T) {
	modes := []string{"valid", "lost", "absent-counter", "null-counter", "duplicate-member", "missing-envelope", "foreign", "unknown-bound", "relay"}
	timing := timingSanitizationFixtures()
	for mode := range timing {
		modes = append(modes, mode)
	}
	slices.Sort(modes)
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			input := rawPoolSample()
			switch mode {
			case "lost":
				input.Phases[0].Receipts[0].Result.Lost = true
			case "absent-counter":
				editCallField(t, &input, "DecryptionFailures", nil)
			case "null-counter":
				editCallField(t, &input, "DecryptionFailures", json.RawMessage("null"))
			case "duplicate-member":
				input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"type":"call"`, `"type":"call","type":"call"`, 1)
			case "missing-envelope":
				input.Missing = true
			case "foreign":
				input.Phases[0].RunID = "foreign-private-identity"
			case "unknown-bound":
				replaceCall(t, &input, func(c *callharness.Report) {
					c.Moves[0].Distributed.Durations["private-metric"] = callharness.DurationInterval{Lower: -1}
				})
			case "relay":
				in := relayInput()
				input = previewPoolInput{RunID: in.RunID, Phases: []previewMediaInput{in}}
			}
			if fixture, ok := timing[mode]; ok {
				replaceCall(t, &input, func(c *callharness.Report) {
					switch fixture.target {
					case "first_content":
						c.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{fixture.reason}}
					case "first_new_content":
						c.Moves[0].Measurement.FirstNewContentVerdict = callharness.MetricVerdict{Reasons: []string{fixture.reason}}
					case "distributed_window":
						c.Moves[0].Distributed.WindowOutcome = "inconclusive"
						c.Moves[0].Distributed.Causes = []string{fixture.reason}
					case "clock_error":
						c.Distributed.ClockError = fixture.reason
					}
				})
			}
			raw, _ := json.Marshal(input)
			original, err := decodePool(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := pooledPreviewMedia(&original)
			if fixture, ok := timing[mode]; ok {
				canonical, recognized := observe.CanonicalTimingReason(fixture.reason)
				if recognized {
					again, ok := observe.CanonicalTimingReason(canonical)
					if !ok || again != canonical || timingCause(canonical) != fixture.cause {
						t.Fatal("canonical cause or precedence changed", fixture, canonical)
					}
				} else if fixture.cause != "trusted-coverage-below-80-percent" {
					t.Fatal("recognized cause has no canonical reason", fixture)
				}
				found := false
				metric := fixture.target
				if metric == "clock_error" {
					metric = "distributed_window"
				}
				for _, row := range want.UndecidedMetrics {
					if row.Metric == metric && slices.Contains(row.Causes, fixture.cause) {
						found = true
					}
				}
				if want.InconclusiveMetrics == 0 || len(want.UndecidedMetrics) != want.InconclusiveMetrics || !found {
					t.Fatal("fixture did not exercise timing cause", fixture.cause, want)
				}
			}
			var pool operatorevidence.Pool
			if err = json.Unmarshal(raw, &pool); err != nil {
				t.Fatal(err)
			}
			// The root pool is already typed at the recorder; caller JSONL must retain
			// the exact missing/null/duplicate product shape rather than fill zeros.
			clean, err := operatorrehearsal.SanitizeMediaEvidence(pool)
			if err != nil {
				t.Fatal(err)
			}
			sanitized, err := decodePool(clean)
			if err != nil {
				t.Fatal(err)
			}
			got := pooledPreviewMedia(&sanitized)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("MUTATION: timing-reason-sanitization: sanitization changed judge result: got %+v want %+v", got, want)
			}
			if strings.Contains(string(clean), "run-fixture") || strings.Contains(string(clean), "session") && strings.Contains(string(clean), `"session":"session"`) {
				t.Fatal("identity retained")
			}
		})
	}
}
func editCallField(t *testing.T, input *previewPoolInput, key string, value json.RawMessage) {
	t.Helper()
	lines := strings.Split(input.Phases[0].CallerJSONL, "\n")
	for i, line := range lines {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &m) != nil || string(m["type"]) != `"call"` {
			continue
		}
		var c map[string]json.RawMessage
		_ = json.Unmarshal(m["call"], &c)
		if value == nil {
			delete(c, key)
		} else {
			c[key] = value
		}
		m["call"], _ = json.Marshal(c)
		raw, _ := json.Marshal(m)
		lines[i] = string(raw)
	}
	input.Phases[0].CallerJSONL = strings.Join(lines, "\n")
}
func TestInvalidPhaseNamesFirstContractAndKeepsHealthySibling(t *testing.T) {
	input := rawPoolSample()
	editCallField(t, &input, "DecryptionFailures", nil)
	got := pooledPreviewMedia(&input)
	issue := got.PhaseDiagnostics[0].Issue
	if issue.Reason != "missing-or-null" || !strings.HasSuffix(issue.Path, ".call.DecryptionFailures") || got.IncludedPhases != 1 || !got.PhaseDiagnostics[1].Included {
		t.Fatal(got, issue)
	}
	input.Phases[0].Receipts[0].Result.Lost = true
	if failed := pooledPreviewMedia(&input); failed.Status != "failed" {
		t.Fatal("known loss disappeared", failed)
	}
}
func TestDecodePoolQuarantinesOnlyIncompletePhase(t *testing.T) {
	input := rawPoolSample()
	raw, _ := json.Marshal(input)
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	var phases []map[string]json.RawMessage
	_ = json.Unmarshal(wire["phases"], &phases)
	delete(phases[0], "events")
	wire["phases"], _ = json.Marshal(phases)
	raw, _ = json.Marshal(wire)
	decoded, err := decodePool(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := pooledPreviewMedia(&decoded)
	if got.IncludedPhases != 1 || got.DroppedPhases != 1 || got.PhaseDiagnostics[0].Issue.Path != "$.events" || got.PhaseDiagnostics[0].Issue.Reason != "missing-or-null" {
		t.Fatal(got)
	}
}

func TestAmbiguousPhaseDoesNotSuppressHealthySibling(t *testing.T) {
	raw, _ := json.Marshal(rawPoolSample())
	// Inject a duplicate member inside one phase, leaving the outer run ID alone.
	first := strings.Index(string(raw), `"phases":[`)
	prefix, phases := string(raw[:first]), string(raw[first:])
	phases = strings.Replace(phases, `"runId":"run-fixture"`, `"runId":"foreign","runId":"run-fixture"`, 1)
	decoded, err := decodePool([]byte(prefix + phases))
	if err != nil {
		t.Fatal(err)
	}
	got := pooledPreviewMedia(&decoded)
	if got.IncludedPhases != 1 || got.DroppedPhases != 1 || got.PhaseDiagnostics[0].Issue.Reason != "duplicate-json-key" {
		t.Fatal(got)
	}
}
func TestInvalidCallRetainsKnownDecryptionFailure(t *testing.T) {
	input := rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 7 })
	editCallField(t, &input, "Offer", nil)
	got := pooledPreviewMedia(&input)
	if got.Status != "failed" || got.DroppedPhases != 1 || got.IncludedPhases != 1 {
		t.Fatal(got)
	}
}

// Each family enters through product-owned reason fields, not a prebuilt summary.
func timingSanitizationFixtures() map[string]struct{ target, reason, cause string } {
	fixtures := map[string]struct{ target, reason, cause string }{}
	families := []struct{ name, reason, cause string }{
		{"varies", "private-identity classification changes within event-time interval; width=12ms", "classification-varies-with-clock-interval"},
		{"unproven", "private-identity not proven throughout event-time interval; width=12ms", "classification-unproven-within-clock-interval"},
		{"evaluation", "interval evaluation unavailable or exceeds 4096 evaluations; width=12ms", "interval-evaluation-unavailable"},
		{"bounds", "duration=[1ms,12ms]; event interval width=12ms", "duration-bounds-uncertain"},
	}
	for _, f := range families {
		for _, target := range []string{"first_content", "first_new_content"} {
			fixtures["timing-"+target+"-"+f.name] = struct{ target, reason, cause string }{target, f.reason, f.cause}
		}
	}
	distributed := []struct{ name, reason, cause string }{
		{"window", "event window evidence incomplete", "event-window-incomplete"},
		{"baseline", "private-identity gap baseline has fewer than 10 guaranteed intervals", "baseline-intervals-insufficient"},
		{"pause", "private-identity pause may reach 2s within clock interval", "pause-bound-uncertain"},
		{"spread", "private-identity baseline spread not proven within 50ms", "baseline-spread-uncertain"},
		{"p95", "private-identity move p95 policy not proven across clock interval", "move-p95-unproven"},
		{"count", "too few distributed trials: moves 19/20, takeovers 9/10", "trial-count-insufficient"},
		{"content", "private-identity: content loss not proven across clock interval", "content-loss-unproven"},
	}
	for _, f := range distributed {
		fixtures["timing-distributed-"+f.name] = struct{ target, reason, cause string }{"distributed_window", f.reason, f.cause}
	}
	for _, target := range []string{"first_content", "first_new_content", "distributed_window"} {
		fixtures["timing-unknown-"+target] = struct{ target, reason, cause string }{target, "unrecognized private-identity reason", "trusted-coverage-below-80-percent"}
	}
	fixtures["timing-clock-error"] = struct{ target, reason, cause string }{"clock_error", families[1].reason, families[1].cause}
	fixtures["timing-overlapping-families"] = struct{ target, reason, cause string }{"first_content", "interval evaluation unavailable or exceeds 4096 evaluations; classification changes within event-time interval; width=12ms", families[0].cause}
	return fixtures
}
