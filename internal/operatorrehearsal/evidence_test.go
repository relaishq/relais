package operatorrehearsal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

func TestSanitizedBoundReceipt(t *testing.T) {
	raw, err := os.ReadFile("../operatorgates/clocks/testdata/bound-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	var s clocks.ClockSelection
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	e, err := (clocks.CoverageEvidence{FrontendSlot: s.Receipt.FrontendSlot, ControlSlot: s.Receipt.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, Bounds: s.Receipt.Bounds}, s.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"matching", "unbound-pool", "wrong-run"} {
		pool := operatorevidence.Pool{RunID: s.Receipt.RunID, ClockReceiptSHA256: s.SHA256, ClockCoverage: &e}
		if mode == "unbound-pool" {
			pool.ClockReceiptSHA256 = ""
		}
		if mode == "wrong-run" {
			pool.RunID = "other-run"
		}
		_, err := SanitizeMediaEvidence(pool)
		if (mode == "matching") != (err == nil) {
			t.Fatal("MUTATION: sanitized-bound-receipt", mode, err)
		}
	}
}

func TestEvidenceArtifactScrubsTextWithoutFillingMissingFields(t *testing.T) {
	secret := "arbitrary-credential-value"
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	stream := `{"type":"call","pid":42,"session":"private-session","call":{"Offer":"a=ice-pwd:` + secret + `","StartedAt":"` + timestamp + `","DecryptionFailures":{"AuthTag":7,"Replay":0,"Other":0},"ICERestarts":0,"private-account-id":123456789012,"stats":{"interval_ns":123456789012},"durations":{"covert":{"lower_ns":123456789012}},"null":null,"duplicate":1,"duplicate":2}}` + "\n" + `{"type":"done","pid":42}` + "\n"
	pool := operatorevidence.Pool{SourceCommit: ReleaseACommit, RunID: "private-run", Phases: []operatorevidence.MediaInput{{Gate: operator.Rollout, RunID: "private-run", Sessions: []string{"private-session"}, RawMedia: operatorevidence.RawMedia{CallerJSONL: stream}}}}
	clean, err := SanitizeMediaEvidence(pool)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip operatorevidence.Pool
	if err = json.Unmarshal(clean, &roundtrip); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{secret, timestamp, "private-session", "private-run", "private-account-id", "123456789012"} {
		if strings.Contains(string(clean), unsafe) {
			t.Fatal("private evidence retained", unsafe)
		}
	}
	if !strings.Contains(roundtrip.Phases[0].CallerJSONL, `"AuthTag":7`) || strings.Contains(roundtrip.Phases[0].CallerJSONL, `"HungUpAt"`) {
		t.Fatal("sanitizer changed failure counter or filled missing evidence")
	}
	if strings.Count(roundtrip.Phases[0].CallerJSONL, `"`+evidenceAlias("duplicate")+`":`) != 2 {
		t.Fatal("duplicate members disappeared")
	}
	output := t.TempDir()
	if err = saveMediaEvidence(output, "media-evidence.json", pool, strings.Repeat("b", 64), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(output, "media-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		SchemaVersion int                   `json:"schemaVersion"`
		SourceCommit  string                `json:"sourceCommit"`
		Pool          operatorevidence.Pool `json:"pool"`
	}
	if err = json.Unmarshal(raw, &artifact); err != nil || artifact.SchemaVersion != 1 || artifact.SourceCommit != ReleaseACommit || len(artifact.Pool.Phases) != 1 {
		t.Fatal("artifact not rejudgeable", err)
	}
	if strings.Contains(string(raw), "headCaller") {
		t.Fatal("pinned evidence names a head caller build")
	}
}

func TestHeadEvidenceRefusesAnUnnamedBuild(t *testing.T) {
	pool := operatorevidence.Pool{SourceCommit: ReleaseACommit}
	for _, head := range []*headCallerBuild{{"abc", strings.Repeat("c", 64)}, {strings.Repeat("a", 40), "c"}} {
		output := t.TempDir()
		if err := saveMediaEvidence(output, HeadCallerEvidence, pool, strings.Repeat("b", 64), head); err == nil || err.Error() != "invalid-evidence-provenance" {
			t.Fatal("unnamed head build accepted", head, err)
		}
		if _, err := os.Stat(filepath.Join(output, HeadCallerEvidence)); !os.IsNotExist(err) {
			t.Fatal("refused evidence was written", err)
		}
	}
}
func TestEvidenceArtifactPreservesNumbersAndTimestampDifferences(t *testing.T) {
	base := time.Now().UTC()
	later := base.Add(109*time.Second + 3*time.Nanosecond)
	stream := `{"type":"stats","pid":42,"stats":{"at":"` + base.Format(time.RFC3339Nano) + `","interval_ns":1203035768,"value":9007199254740993}}` + "\n" + `{"type":"stats","pid":42,"stats":{"at":"` + later.Format(time.RFC3339Nano) + `"}}` + "\n"
	pool := operatorevidence.Pool{RunID: "private", Phases: []operatorevidence.MediaInput{{RawMedia: operatorevidence.RawMedia{CallerJSONL: stream}}}}
	raw, err := SanitizeMediaEvidence(pool)
	if err != nil {
		t.Fatal(err)
	}
	var decoded operatorevidence.Pool
	_ = json.Unmarshal(raw, &decoded)
	lines := strings.Split(decoded.Phases[0].CallerJSONL, "\n")
	var a, b loadgen.Message
	if json.Unmarshal([]byte(lines[0]), &a) != nil || json.Unmarshal([]byte(lines[1]), &b) != nil || b.Stats.At.Sub(a.Stats.At) != later.Sub(base) {
		t.Fatal("timestamp differences changed")
	}
	if !strings.Contains(decoded.Phases[0].CallerJSONL, "1203035768") {
		t.Fatal("number token changed")
	}
}

func TestRecordingJudgeRetainsInputAndLogsPublicContract(t *testing.T) {
	output := t.TempDir()
	summary := operatorevidence.Summary{Status: operator.Inconclusive, PhaseDiagnostics: []operatorevidence.PhaseDiagnostic{{Phase: 0, Cause: "invalid-phase-evidence", Issue: observe.EvidenceIssue{Path: "$.callerJsonl[12].call.DecryptionFailures", Reason: "missing-or-null"}}}}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "judge")
	script := []byte("#!/bin/sh\nprintf '%s\\n' '" + string(raw) + "'\n")
	if err = os.WriteFile(binary, script, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	var log bytes.Buffer
	retained := false
	judge := recordingJudge{pinned: operatorevidence.PinnedJudge{Commit: ReleaseACommit, Binary: binary, SHA256: hex.EncodeToString(sum[:])}, diagnostics: &log, retain: func(p operatorevidence.Pool) error {
		retained = true
		return saveMediaEvidence(output, "media-evidence.json", p, hex.EncodeToString(sum[:]), nil)
	}}
	if _, err = judge.Judge(context.Background(), operatorevidence.Pool{SourceCommit: ReleaseACommit, RunID: "private-run"}); err != nil {
		t.Fatal(err)
	}
	if !retained || !strings.Contains(log.String(), "contract_path=$.callerJsonl[12].call.DecryptionFailures reason=missing-or-null") {
		t.Fatal("real recorder omitted contract", log.String())
	}
	if _, err = os.Stat(filepath.Join(output, "media-evidence.json")); err != nil {
		t.Fatal("real recorder lost judge input", err)
	}
}

func TestEvidenceTimingReasonsRemainPrivateAndClassifiable(t *testing.T) {
	reason := "private-customer-token classification changes within event-time interval; width=12ms"
	canonical, ok := observe.CanonicalTimingReason(reason)
	if !ok {
		t.Fatal("missing canonical reason")
	}
	message := loadgen.Message{Type: "call", PID: 42, Session: reason, Call: &callharness.Report{
		Offer:       reason,
		Distributed: &callharness.DistributedRecording{ClockError: reason, Causes: []string{reason}},
		Moves:       []callharness.MoveReport{{Measurement: callharness.EventMeasurement{FirstContentVerdict: callharness.MetricVerdict{Reasons: []string{reason}}}, Distributed: &callharness.DistributedMeasurement{Causes: []string{reason}}}},
	}}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	pool := operatorevidence.Pool{Phases: []operatorevidence.MediaInput{{RawMedia: operatorevidence.RawMedia{CallerJSONL: string(raw) + "\n"}}}}
	clean, err := SanitizeMediaEvidence(pool)
	if err != nil {
		t.Fatal(err)
	}
	var saved operatorevidence.Pool
	if err = json.Unmarshal(clean, &saved); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(saved.Phases[0].CallerJSONL), &message); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{message.Call.Moves[0].Measurement.FirstContentVerdict.Reasons[0], message.Call.Moves[0].Distributed.Causes[0], message.Call.Distributed.ClockError, message.Call.Distributed.Causes[0]} {
		if got != canonical || observe.TimingCause(got) != observe.TimingCause(reason) {
			t.Fatal("timing reason changed", got)
		}
	}
	if message.Session != evidenceAlias(reason) || message.Call.Offer != evidenceAlias(reason) {
		t.Fatal("identity or SDP interpreted as a reason")
	}
	for _, private := range []string{"private-customer-token", "12ms"} {
		if strings.Contains(string(clean), private) {
			t.Fatal("dynamic reason text escaped", private)
		}
	}
	// A field named Reasons in an unknown extension has no product ownership.
	s := evidenceSanitizer{fields: map[reflect.Type]map[string]reflect.Type{}}
	unknown, err := s.document([]byte(`{"Reasons":["`+reason+`"]}`), nil)
	if err != nil || strings.Contains(string(unknown), canonical) {
		t.Fatal("unknown extension retained a reason", err)
	}
	for _, unrecognized := range []string{"unknown-private-reason", "classification-varies-with-clock-interval", "classification changes within event-time interval", ""} {
		got := s.text(unrecognized, true)
		if observe.TimingCause(got) != observe.TimingCause(unrecognized) || unrecognized != "" && got != evidenceAlias(unrecognized) {
			t.Fatal("unknown cause changed", unrecognized, got)
		}
	}
}
