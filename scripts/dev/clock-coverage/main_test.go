package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

func TestOfflineCoverageReportsMissingStoredRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.json")
	e := clocks.CoverageEvidence{Version: 2, FrontendSlot: "shared", ControlSlot: "shared", Windows: []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010250000000", EndUnixNS: "1793930020250000000"}}}
	raw, _ := json.Marshal(map[string]any{"clockCoverage": e})
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := evaluate([]string{"-report", p}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"clock-coverage-missing":1`) || !strings.Contains(out.String(), `"covered":0`) {
		t.Fatal(out.String())
	}
	before := out.String()
	out.Reset()
	if err := evaluate([]string{"-report", p}, &out); err != nil || out.String() != before {
		t.Fatal("nondeterministic offline output", err, out.String())
	}
}

func TestHistoricalTrackingReplayAlignsClocksAndKeepsBootBreakLocal(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	poolPath := filepath.Join(dir, "pool.json")
	base := time.Unix(1793930000, 0).UTC()
	sanitized := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	starts := []time.Duration{1500 * time.Millisecond, 10*time.Second + 200*time.Microsecond, 11500*time.Millisecond + 700*time.Microsecond}

	raw := operatorevidence.RawMedia{}
	timing := []map[string]any{}
	for i, offset := range starts {
		kind, receiptKind := "move", "move"
		if i == 2 {
			kind, receiptKind = "kill", "takeover"
		}
		id := callharness.OperationIdentity{RunID: "synthetic", OperationID: fmt.Sprint(i), SessionID: "one", Worker: "opaque-worker-alias", Epoch: 1}
		start := sanitized.Add(offset)
		end := start.Add(100 * time.Millisecond)
		raw.Events = append(raw.Events, loadgen.EventResult{Event: loadgen.Event{Kind: kind, Worker: 1}, At: start, FinishedAt: end, Identities: []callharness.OperationIdentity{id}})
		raw.Receipts = append(raw.Receipts, operatorcaller.Receipt{Identity: id, Kind: receiptKind, Start: start, End: end})
		timing = append(timing, map[string]any{"kind": receiptKind, "startUnixMs": base.Add(offset).UnixMilli()})
	}
	pool := operatorevidence.Pool{Phases: []operatorevidence.MediaInput{{Gate: operator.Rollout, RawMedia: raw}}}
	rows := []clocks.TrackingRow{{UnixSeconds: base.Unix(), LeapStatus: "?", MaxErrorSeconds: 1.5, BeforeRun: true}, {UnixSeconds: base.Unix() + 4, LeapStatus: "N", MaxErrorSeconds: .00001}}
	for _, n := range []int64{9, 12, 15} {
		rows = append(rows, clocks.TrackingRow{UnixSeconds: base.Unix() + n, LeapStatus: "N", MaxErrorSeconds: .00001})
	}
	samples := map[string]clocks.Sample{}
	for _, slot := range []string{"shared", "w1"} {
		samples[slot] = clocks.Sample{TrackingLog: &clocks.TrackingLog{Rows: rows, Breaks: []clocks.TrackingBreak{{FromUnixSeconds: base.Unix(), UnixSeconds: base.Unix(), Kind: "leap-status", BeforeRun: true}}}}
	}
	attempt := clocks.Evidence{Version: 2, Clocks: samples}
	report := map[string]any{"bounds": clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000}, "clocks": []clocks.Evidence{attempt, attempt}, "callerTiming": []map[string]any{{"gate": "rollout", "events": timing}}}
	write := func(path string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(poolPath, map[string]any{"pool": pool})
	write(reportPath, report)
	replay := func() struct {
		Events, Covered        int
		Results                []eventResult
		TimestampUncertaintyNS int64
	} {
		t.Helper()
		var out bytes.Buffer
		if err := evaluate([]string{"-report", reportPath, "-pool", poolPath, "-frontend-slot", "shared", "-control-slot", "shared"}, &out); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Events, Covered        int
			Results                []eventResult
			TimestampUncertaintyNS int64
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := replay()
	if got.Events != 3 || got.Covered != 2 || len(got.Results[0].Causes) != 1 || got.Results[0].Causes[0] != "clock-coverage-break" {
		t.Fatal("boot evidence leaked outside its interval", got)
	}
	if got.TimestampUncertaintyNS != 299999 || got.Results[2].KillBracketRule != "unavailable" || len(got.Results[2].Causes) != 0 {
		t.Fatal("unavailable bracket became a cause or clock alignment lost precision", got)
	}
	// A repeated second must preserve its largest envelope, even when a smaller
	// copy appears later. It may not be mistaken for a later repair sample.
	log := samples["shared"].TrackingLog
	log.Rows = append(append([]clocks.TrackingRow{}, rows...), clocks.TrackingRow{UnixSeconds: base.Unix() + 12, LeapStatus: "N", MaxErrorSeconds: .003}, rows[3])
	write(reportPath, report)
	got = replay()
	if !slices.Contains(got.Results[1].Causes, "clock-coverage-bound") {
		t.Fatal("duplicate-second maximum was lost", got)
	}
}
