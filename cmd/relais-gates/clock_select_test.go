package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/stretchr/testify/require"
)

func TestClockSelectCommandRecordsReceiptWithoutSession(t *testing.T) {
	const start int64 = 1792886402
	in := clocks.SelectionEvidence{RunID: "cli-test", FrontendSlot: "shared", ControlSlot: "shared", CallerLeapMode: "unsmeared", StartUnixSeconds: start, EndUnixSeconds: start + 120}
	for _, slot := range []string{"relay", "shared", "w1", "w2"} {
		h := clocks.SelectionHost{Slot: slot, LeapMode: "unsmeared", Tracking: clocks.TrackingLog{ReadComplete: true}, Lease: clocks.LeaseObservation{Available: true, ErrorBoundNS: 10000, MeasuredUnixNS: "1792886524000000000", ReferenceUnixNS: "1792886524000000000", PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 1e9, HostElapsedNS: 1e9}}
		for at := start - 2; at <= start+121; at++ {
			h.Tracking.Rows = append(h.Tracking.Rows, clocks.TrackingRow{UnixSeconds: at, MaxErrorSeconds: 1e-5, MaxErrorDecimal: "1.000e-05", LeapStatus: "N"})
		}
		in.Hosts = append(in.Hosts, h)
	}
	in.Falsification.FrontendSlot = "shared"
	for at := start; at < start+120; at++ {
		for _, target := range []string{"control", "w1", "w2"} {
			n := at * 1e9
			in.Falsification.Records = append(in.Falsification.Records, clocks.ProbeRecord{Target: target, FrontendSend: fmt.Sprint(n), HostReceive: fmt.Sprint(n + 1000), HostSend: fmt.Sprint(n + 1000), FrontendReceive: fmt.Sprint(n + 2000), Status: "not-falsified"})
		}
	}
	p := clocks.SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "2"}
	dir := t.TempDir()
	evidence, policy := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "policy.json")
	write := func(path string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, b, 0600))
	}
	write(evidence, in)
	write(policy, p)
	var out, diagnostics bytes.Buffer
	args := []string{"-evidence", evidence, "-policy", policy}
	require.Equal(t, 0, clockSelect(args, &out, &diagnostics), diagnostics.String())
	var got clocks.ClockSelection
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Equal(t, int64(120000), got.Receipt.Bounds.FrontendControlBoundNS)
	require.Equal(t, int64(170000), got.Receipt.Bounds.FrontendWorkerBoundNS)
	want, err := clocks.Select(in, p)
	require.NoError(t, err)
	require.Equal(t, want, got)
	original := out.String()
	out.Reset()
	require.Equal(t, 0, clockSelect(args, &out, &diagnostics))
	require.Equal(t, original, out.String())
	// Unknown members must refuse even when all required evidence is valid.
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	withUnknown := append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	require.NoError(t, os.WriteFile(evidence, withUnknown, 0600))
	out.Reset()
	require.Equal(t, 1, clockSelect(args, &out, &diagnostics))
	require.Empty(t, out.String())
	write(evidence, in)
	p.ControlCeilingNS = 1
	write(policy, p)
	out.Reset()
	require.Equal(t, 1, clockSelect(args, &out, &diagnostics))
	require.Empty(t, out.String())
	require.Contains(t, diagnostics.String(), "clock-select-ceiling")
}

func TestClockSelectCommandRejectsIncompleteOrAmbiguousInput(t *testing.T) {
	var out, diagnostics bytes.Buffer
	require.Equal(t, 2, clockSelect(nil, &out, &diagnostics))
	p := filepath.Join(t.TempDir(), "input.json")
	for _, raw := range []string{`{}`, `{"runId":"a","runId":"b"}`, `{"unknown":1}`, `[]`, `{} {}`} {
		require.NoError(t, os.WriteFile(p, []byte(raw), 0600))
		require.Equal(t, 1, clockSelect([]string{"-evidence", p, "-policy", p}, &out, &diagnostics))
		require.Empty(t, out.String())
	}
}
