package clocks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	before, err := Check(rows, bounds, "relay", "shared")
	if err != nil {
		t.Fatal(err)
	}
	for slot, sample := range rows {
		sample.TrackingLog = &TrackingLog{Breaks: []TrackingBreak{{Kind: "self-consistency"}}}
		rows[slot] = sample
	}
	after, err := Check(rows, bounds, "relay", "shared")
	if err != nil || before.Control != after.Control || before.Worker != after.Worker || before.Drift != after.Drift || after.Version != 2 {
		t.Fatal("recorded tracking changed check", after, err)
	}
}

func TestCheckUsesComponentSlots(t *testing.T) {
	for _, tc := range []struct {
		name, frontend, control, largeSlot string
		wantControl, wantWorker            int64
		wantDrift                          float64
		wantError                          bool
	}{
		{"shared frontend", "shared", "shared", "", 4, 6, 6, false},
		{"large shared", "shared", "shared", "shared", 40, 24, 40, true},
		{"large relay excluded", "shared", "shared", "relay", 4, 6, 6, false},
		{"relay frontend", "relay", "shared", "", 3, 5, 5, false},
		{"relay frontend large shared", "relay", "shared", "shared", 21, 5, 21, true},
		{"relay frontend large relay", "relay", "shared", "relay", 22, 24, 24, true},
		{"relocated control", "shared", "relay", "relay", 22, 6, 22, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := map[string]Sample{
				"relay": {ErrorBoundNS: 1, RateErrorPPM: 1}, "shared": {ErrorBoundNS: 2, RateErrorPPM: 2},
				"w1": {ErrorBoundNS: 3, RateErrorPPM: 3}, "w2": {ErrorBoundNS: 4, RateErrorPPM: 4},
			}
			if tc.largeSlot != "" {
				rows[tc.largeSlot] = Sample{ErrorBoundNS: 20, RateErrorPPM: 20}
			}
			e, err := Check(rows, Bounds{FrontendControlBoundNS: 10, FrontendWorkerBoundNS: 10, DriftPPM: 10}, tc.frontend, tc.control)
			if e.Control != tc.wantControl || e.Worker != tc.wantWorker || e.Drift != tc.wantDrift || (err != nil) != tc.wantError {
				t.Fatalf("component clock bounds: %+v, %v", e, err)
			}
			if err != nil && err.Error() != "measured clocks exceed approved bounds" {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckRequiresSlotsAndAllFourRows(t *testing.T) {
	for _, tc := range []struct{ name, frontend, control, missing string }{
		{"missing frontend", "", "shared", ""},
		{"missing control", "shared", "", ""},
		{"unknown frontend", "unknown", "shared", ""},
		{"unknown control", "shared", "unknown", ""},
		{"missing relay row", "shared", "shared", "relay"},
		{"missing shared row", "relay", "relay", "shared"},
		{"missing w1 row", "shared", "shared", "w1"},
		{"missing w2 row", "shared", "shared", "w2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := map[string]Sample{"relay": {}, "shared": {}, "w1": {}, "w2": {}}
			delete(rows, tc.missing)
			if _, err := Check(rows, Bounds{}, tc.frontend, tc.control); err == nil || err.Error() != "missing independent clock evidence" {
				t.Fatalf("missing evidence accepted: %v", err)
			}
		})
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

func TestSignalBracketRangeAndEnvelope(t *testing.T) {
	for _, n := range []int64{2000000000, 9223372034854775807} {
		stamp := fmt.Sprintf("%019d", n)
		at := time.Unix(0, n)
		got, err := SignalMidpoint(stamp, stamp, 0, 1, at, at)
		if err != nil || got != stamp {
			t.Fatal("MUTATION: bracket-range-boundary", stamp, got, err)
		}
	}
	for _, before := range []string{"bad", "0000000000000000000", "0000000001999999999", "9223372034854775808"} {
		_, err := SignalMidpoint(before, "1793930000000000000", 0, 170000)
		if err == nil || errors.Is(err, ErrSignalBracketWide) {
			t.Fatal("MUTATION: bracket-malformed-wide", before, err)
		}
	}
}
