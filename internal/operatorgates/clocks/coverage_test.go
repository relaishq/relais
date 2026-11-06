package clocks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func TestCoveragePublicProjection(t *testing.T) {
	secret := "private-clock-source"
	source := Evidence{Version: 2, Clocks: map[string]Sample{secret: {}, "shared": {Tracking: secret, MeasuredUnixNS: secret, TrackingLog: &TrackingLog{Cursor: json.RawMessage(`{"file":"private-clock-source","position":42,"anchor":"private-clock-source","daemon":"private-clock-source"}`), Rows: []TrackingRow{{Reference: secret, LeapStatus: secret}}, Breaks: []TrackingBreak{{Kind: secret}}}}}}
	e := CoverageEvidence{Version: 2, FrontendSlot: secret, ControlSlot: "shared", Attempts: []CoverageAttempt{NewCoverageAttempt(source)}, KillBrackets: []observe.CoverageBracket{{Worker: secret, BeforeUnixNS: secret}}, Windows: []observe.CoverageWindow{{Kind: secret, Worker: secret, StartUnixNS: secret}}, Results: []observe.CoverageResult{{Causes: []string{secret}}}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"reference"`) || strings.Contains(string(raw), `"cursor"`) {
		t.Fatal("private coverage retained", string(raw))
	}
	if source.Clocks["shared"].Tracking != secret {
		t.Fatal("projection mutated source")
	}
	again, _ := json.Marshal(e)
	if string(raw) != string(again) {
		t.Fatal("projection is nondeterministic")
	}
}
