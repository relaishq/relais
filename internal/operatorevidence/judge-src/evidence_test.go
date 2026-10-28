package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestSanitizedArtifactRejudgesRawPhases(t *testing.T) {
	for _, mode := range []string{"valid", "lost", "absent-counter", "null-counter", "duplicate-member", "missing-envelope", "foreign", "unknown-bound", "relay"} {
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
			raw, _ := json.Marshal(input)
			original, err := decodePool(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := pooledPreviewMedia(&original)
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
				t.Fatalf("sanitization changed judge result: got %+v want %+v", got, want)
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
