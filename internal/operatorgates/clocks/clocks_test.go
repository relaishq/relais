package clocks

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/oracletest"
)

func TestSignalBracketOracle(t *testing.T) {
	for i, c := range oracletest.Cases(t, "clocks.verifySignalBracket") {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var data struct {
				Before string `json:"signalBeforeUnixNs"`
				After  string `json:"signalAfterUnixNs"`
			}
			_ = json.Unmarshal(c.Request.Inputs[0], &data)
			got, err := SignalMidpoint(data.Before, data.After, oracletest.JSON[int64](t, c.Request.Inputs[1]), oracletest.JSON[int64](t, c.Request.Inputs[2]))
			if (err == nil) != (c.Result.Completion == "returned") {
				t.Fatalf("ignore-signal-bracket-width: oracle verdict %v", err)
			}
			if err == nil && got != oracletest.JSON[string](t, c.Result.Value) {
				t.Fatalf("signal midpoint differs: %s", got)
			}
		})
	}
}

func TestTrackingBreaksDoNotChangeClockCheck(t *testing.T) {
	rows := map[string]Sample{}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		rows[slot] = Sample{ErrorBoundNS: 1, RateErrorPPM: 0.1}
	}
	bounds := Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 1}
	before, err := Check(rows, bounds)
	if err != nil {
		t.Fatal(err)
	}
	for slot, sample := range rows {
		sample.TrackingLog = &TrackingLog{Breaks: []TrackingBreak{{Kind: "self-consistency"}}}
		rows[slot] = sample
	}
	after, err := Check(rows, bounds)
	if err != nil || before.Control != after.Control || before.Worker != after.Worker || before.Drift != after.Drift || after.Version != 2 {
		t.Fatal("recorded tracking changed check", after, err)
	}
}

func TestPublicTrackingRetainsTimelineWithoutRawCursorOrSource(t *testing.T) {
	source := Sample{Tracking: "private-label", TrackingLog: &TrackingLog{
		Cursor: json.RawMessage(`{"position":128,"anchor":"abc","private-label":"private-label"}`),
		Rows:   []TrackingRow{{Reference: "203.0.113.15", MaxErrorSeconds: 0.000009, BeforeRun: true}},
		Breaks: []TrackingBreak{{Kind: "source-change", UnixSeconds: 1}},
	}}
	public := PublicSample(source)
	raw, err := json.Marshal(public)
	if err != nil || strings.Contains(string(raw), "private-label") || strings.Contains(string(raw), `"reference"`) || !public.TrackingLog.Rows[0].BeforeRun || !strings.Contains(string(raw), `"position":128`) || public.TrackingLog.Rows[0].MaxErrorSeconds != 0.000009 || len(public.TrackingLog.Breaks) != 1 {
		t.Fatal(string(raw), err)
	}
	if source.TrackingLog.Rows[0].Reference != "203.0.113.15" {
		t.Fatal("public projection mutated the cursor source")
	}
}
