package operator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestScenarioIdentityUnbound(t *testing.T) {
	s := Scenario{SchemaVersion: 1, Gates: []Gate{Rollout}, Template: Identity{SourceCommit: "source", SHA256: "template"}}
	// Fixed pre-binding encoding, including field order and the empty digest.
	want := `{"schemaVersion":1,"digest":"","gates":["rollout"],"template":{"sourceCommit":"source","sha256":"template"},"releaseA":{"sourceCommit":"","sha256":""},"releaseB":{"sourceCommit":"","sha256":""},"judge":{"sourceCommit":"","sha256":""},"caller":{"sourceCommit":"","sha256":""}}`
	raw, err := ScenarioIdentityBytes(s)
	wire, wireErr := json.Marshal(s)
	if err != nil || wireErr != nil || string(raw) != want || string(wire) != want {
		t.Fatal("MUTATION: scenario-unbound-bytes", string(raw), string(wire), err, wireErr)
	}
	built, err := BuildScenarioIdentity(s, "")
	if err != nil || built.Digest != "92015d9a125f3452f6c6554856c3b34a45b4da80370f3ba2c657005cc1a810e0" {
		t.Fatal("MUTATION: scenario-unbound-digest", built, err)
	}
}

func TestScenarioIdentityBindsReceipt(t *testing.T) {
	s := Scenario{SchemaVersion: 1, Digest: "previous", Gates: []Gate{Rollout}}
	a, err := BuildScenarioIdentity(s, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildScenarioIdentity(s, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ScenarioIdentityBytes(a)
	if err != nil || a.Digest == b.Digest || a.ClockReceiptSHA256 != strings.Repeat("a", 64) || !bytes.Contains(raw, []byte(`"clockReceiptSha256":"`+strings.Repeat("a", 64)+`"`)) || bytes.Contains(raw, []byte("previous")) {
		t.Fatal("MUTATION: scenario-receipt-binding", a, b, err)
	}
	// A later head-caller identity rebuild preserves the frozen commitment.
	a.Caller.SHA256 = "head-caller"
	head, err := BuildScenarioIdentity(a, a.ClockReceiptSHA256)
	if err != nil || head.Digest == a.Digest || head.ClockReceiptSHA256 != a.ClockReceiptSHA256 {
		t.Fatal("MUTATION: scenario-receipt-retention", head, err)
	}
	head.Gates[0] = Load
	if s.Gates[0] != Rollout || a.Gates[0] != Rollout || s.Digest != "previous" || s.ClockReceiptSHA256 != "" {
		t.Fatal("scenario builder mutated its input")
	}
	for _, bad := range []string{"bad", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		if _, err := BuildScenarioIdentity(s, bad); err == nil {
			t.Fatal("MUTATION: scenario-invalid-receipt", bad)
		}
	}
}
