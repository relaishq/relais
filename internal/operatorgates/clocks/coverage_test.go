package clocks

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
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

func TestCoverageDiagnosticsAndQualificationUseFixedBounds(t *testing.T) {
	for _, bound := range []int64{1000000, 4000000} {
		e := CoverageEvidence{Bounds: FixedCoverageBounds(), FrontendSlot: "shared", ControlSlot: "shared", Windows: []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}}, Attempts: []CoverageAttempt{{Version: 2, Clocks: map[string]observe.CoverageTimeline{"shared": {Rows: []observe.CoverageRow{{UnixSeconds: 1793930000, LeapStatus: "N", MaxErrorSeconds: .0001}, {UnixSeconds: 1793930030, LeapStatus: "N", MaxErrorSeconds: .0001}}}}}}}
		e.Bounds.FrontendControlBoundNS = bound
		e.Evaluate()
		q := e.Qualification()
		if !reflect.DeepEqual(e.Results, q.Results) || !slices.Contains(q.Results[0].Causes, "clock-coverage-bound") {
			t.Fatal("diagnostic/verdict bounds diverged", e.Results, q.Results)
		}
	}
}

func TestSignalDiagnosticsPublicBytes(t *testing.T) {
	e := CoverageEvidence{Bounds: FixedCoverageBounds(), KillBrackets: []observe.CoverageBracket{{Worker: "w1", BeforeUnixNS: "1793930010000000000", AfterUnixNS: "1793930010000000100", MidpointUnixNS: "1793930010000000050"}}}
	before, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	e.KillBrackets[0].Diagnostics = &observe.SignalDiagnostics{Priority: "private-source"}
	public := e.Public()
	if public.KillBrackets[0].Diagnostics.Priority != "unavailable" || e.KillBrackets[0].Diagnostics.Priority != "private-source" {
		t.Fatal("diagnostic projection changed source or leaked text")
	}
	after, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err = json.Unmarshal(after, &obj); err != nil {
		t.Fatal(err)
	}
	delete(obj["killBrackets"].([]any)[0].(map[string]any), "signalDiagnostics")
	var old map[string]any
	if err = json.Unmarshal(before, &old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, obj) {
		t.Fatal("existing report fields changed")
	}
	public.KillBrackets[0].Diagnostics = nil
	stripped, err := json.Marshal(public)
	if err != nil || !bytes.Equal(before, stripped) {
		t.Fatal("existing report bytes changed")
	}
}
