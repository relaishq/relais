package clocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func leaseFixture() LeaseObservation {
	return LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1792886524000000000", ReferenceUnixNS: "1792886524000000000", PollSeconds: "1", ResidualPPM: "-0.1", SkewPPM: "0.2", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 1e9, HostElapsedNS: 1e9}
}

func selectionFixture() (SelectionEvidence, SelectionPolicy) {
	start := int64(1792886402)
	in := SelectionEvidence{RunID: "selection-test", FrontendSlot: "relay", ControlSlot: "shared", CallerLeapMode: "unsmeared", StartUnixSeconds: start, EndUnixSeconds: start + 120}
	for _, slot := range selectionSlots {
		h := SelectionHost{Slot: slot, LeapMode: "unsmeared", Tracking: TrackingLog{ReadComplete: true, Rows: []TrackingRow{}, Breaks: []TrackingBreak{}}, Lease: leaseFixture()}
		for at := start - 2; at <= start+121; at++ {
			h.Tracking.Rows = append(h.Tracking.Rows, TrackingRow{BeforeRun: true, UnixSeconds: at, MaxErrorSeconds: 1.234e-6, MaxErrorDecimal: "1.234e-06", LeapStatus: "N"})
		}
		in.Hosts = append(in.Hosts, h)
	}
	in.Falsification = Falsification{FrontendSlot: "relay", CountsByStatus: map[string]int{}, CountsByReason: map[string]int{}}
	for at := start; at < start+120; at++ {
		for _, target := range []string{"control", "w1", "w2"} {
			t := at * 1e9
			in.Falsification.Records = append(in.Falsification.Records, ProbeRecord{Target: target, FrontendSend: fmt.Sprint(t), HostReceive: fmt.Sprint(t + 1000), HostSend: fmt.Sprint(t + 1000), FrontendReceive: fmt.Sprint(t + 2000), Status: "not-falsified"})
		}
	}
	p := SelectionPolicy{WindowSeconds: 120, ScaleNumerator: 1, ScaleDenominator: 3, AllowanceNS: 1, SignalReserveNS: 57, ControlCeilingNS: 1e6, WorkerCeilingNS: 1e6, MaxDispersionGrowthPPM: "1"}
	return in, p
}

func selectGood(t *testing.T, in SelectionEvidence, p SelectionPolicy) ClockSelection {
	t.Helper()
	r, err := Select(in, p)
	require.NoError(t, err)
	return r
}

func TestClockSelectOutwardRounding(t *testing.T) {
	in, p := selectionFixture()
	r := selectGood(t, in, p)
	if r.Receipt.Bounds != (Bounds{FrontendControlBoundNS: 826, FrontendWorkerBoundNS: 883, DriftPPM: 100}) {
		t.Fatal("MUTATION: inward-rounding", r.Receipt.Bounds)
	}
	// The floor applies before allowance; exact ceiling equality is legal.
	p.FloorNS, p.AllowanceNS = 50000, 10000
	p.ControlCeilingNS, p.WorkerCeilingNS = 120000, 120057
	r = selectGood(t, in, p)
	require.Equal(t, int64(120000), r.Receipt.Bounds.FrontendControlBoundNS)
	require.Equal(t, int64(120057), r.Receipt.Bounds.FrontendWorkerBoundNS)
}

func TestClockSelectCeilingRefuses(t *testing.T) {
	for _, worker := range []bool{false, true} {
		in, p := selectionFixture()
		if worker {
			p.WorkerCeilingNS = 882
		} else {
			p.ControlCeilingNS = 825
		}
		if _, err := Select(in, p); err == nil || err.Error() != "clock-select-ceiling" {
			t.Fatal("MUTATION: clip-ceiling", err)
		}
	}
}

func TestClockSelectMaximumIncludesEarlierIntervals(t *testing.T) {
	in, p := selectionFixture()
	in.Hosts[0].Tracking.Rows[3].MaxErrorDecimal = "1.000e-04"
	in.Hosts[0].Tracking.Rows[3].MaxErrorSeconds = 1e-4
	r := selectGood(t, in, p)
	if r.Receipt.Hosts[0].MaximumNS != "100050" {
		t.Fatal("MUTATION: latest-only-M", r.Receipt.Hosts)
	}
}

func TestClockSelectDuplicateUpdatesDoNotCount(t *testing.T) {
	in, p := selectionFixture()
	rows := []TrackingRow{}
	for _, at := range []int64{-1, 0, 15, 30, 45, 60, 75, 90, 105, 120, 121} {
		row := TrackingRow{BeforeRun: true, UnixSeconds: in.StartUnixSeconds + at, MaxErrorSeconds: 1.234e-6, MaxErrorDecimal: "1.234e-06", LeapStatus: "N"}
		for range 20 {
			rows = append(rows, row)
		}
	}
	in.Hosts[0].Tracking.Rows = rows
	if _, err := Select(in, p); err == nil || err.Error() != "clock-select-updates" {
		t.Fatal("MUTATION: duplicate-updates", err)
	}
}

func TestClockSelectUsesBothWorkers(t *testing.T) {
	in, p := selectionFixture()
	for i := range in.Hosts[3].Tracking.Rows {
		in.Hosts[3].Tracking.Rows[i].MaxErrorDecimal = "9.000e-06"
		in.Hosts[3].Tracking.Rows[i].MaxErrorSeconds = 9e-6
	}
	r := selectGood(t, in, p)
	if r.Receipt.Bounds.FrontendWorkerBoundNS != 3472 {
		t.Fatal("MUTATION: ignore-worker2", r.Receipt.Bounds)
	}
}

func TestClockSelectInventorySlots(t *testing.T) {
	in, p := selectionFixture()
	in.FrontendSlot, in.ControlSlot, in.Falsification.FrontendSlot = "shared", "shared", "shared"
	for i := range in.Hosts[0].Tracking.Rows {
		in.Hosts[0].Tracking.Rows[i].MaxErrorDecimal = "9.000e-06"
		in.Hosts[0].Tracking.Rows[i].MaxErrorSeconds = 9e-6
	}
	r := selectGood(t, in, p)
	if r.Receipt.Bounds.FrontendControlBoundNS != 826 || r.Receipt.Bounds.FrontendWorkerBoundNS != 883 {
		t.Fatal("MUTATION: wrong-slot", r.Receipt.Bounds)
	}
}

func TestClockSelectRelocatedControl(t *testing.T) {
	in, p := selectionFixture()
	in.FrontendSlot, in.ControlSlot, in.Falsification.FrontendSlot = "shared", "relay", "shared"
	for i := range in.Hosts[0].Tracking.Rows {
		in.Hosts[0].Tracking.Rows[i].MaxErrorSeconds = 9e-6
		in.Hosts[0].Tracking.Rows[i].MaxErrorDecimal = "9.000e-06"
	}
	for i, r := range in.Falsification.Records {
		if r.Target == "control" {
			n, _ := strconv.ParseInt(r.FrontendSend, 10, 64)
			in.Falsification.Records[i].HostReceive = fmt.Sprint(n + 5000)
			in.Falsification.Records[i].HostSend = fmt.Sprint(n + 5000)
		}
	}
	r, err := Select(in, p)
	if err != nil || r.Receipt.Bounds.FrontendControlBoundNS != 3415 {
		t.Fatal("MUTATION: relocated-control", r, err)
	}
}

func TestClockSelectPostWindowBreakRefuses(t *testing.T) {
	in, p := selectionFixture()
	for n := int64(2); n <= 20; n++ {
		row := in.Hosts[0].Tracking.Rows[len(in.Hosts[0].Tracking.Rows)-1]
		row.UnixSeconds = in.EndUnixSeconds + n
		in.Hosts[0].Tracking.Rows = append(in.Hosts[0].Tracking.Rows, row)
	}
	now := fmt.Sprint((in.EndUnixSeconds + 21) * 1000000000)
	in.Hosts[0].Lease.MeasuredUnixNS, in.Hosts[0].Lease.ReferenceUnixNS = now, now
	in.Hosts[0].Tracking.Breaks = []TrackingBreak{{Kind: "daemon-restart", FromUnixSeconds: in.EndUnixSeconds + 10, UnixSeconds: in.EndUnixSeconds + 11}}
	if _, err := Select(in, p); err == nil || err.Error() != "clock-select-break" {
		t.Fatal("MUTATION: ignore-post-window-break", err)
	}
}

func TestClockSelectPostWindowStateAndFreshness(t *testing.T) {
	for _, field := range []string{"leap", "reference", "expired", "stale"} {
		t.Run(field, func(t *testing.T) {
			in, p := selectionFixture()
			for n := int64(2); n <= 20; n++ {
				row := in.Hosts[0].Tracking.Rows[len(in.Hosts[0].Tracking.Rows)-1]
				row.UnixSeconds = in.EndUnixSeconds + n
				in.Hosts[0].Tracking.Rows = append(in.Hosts[0].Tracking.Rows, row)
			}
			last := len(in.Hosts[0].Tracking.Rows) - 1
			now := fmt.Sprint((in.EndUnixSeconds + 21) * 1000000000)
			in.Hosts[0].Lease.MeasuredUnixNS, in.Hosts[0].Lease.ReferenceUnixNS = now, now
			switch field {
			case "leap":
				in.Hosts[0].Tracking.Rows[last].LeapStatus = "+"
			case "reference":
				in.Hosts[0].Tracking.Rows[last].Reference = "changed-source"
			case "expired":
				in.Hosts[0].Lease.ReferenceAgeNS = 4000000000
				n, _ := strconv.ParseInt(in.Hosts[0].Lease.MeasuredUnixNS, 10, 64)
				in.Hosts[0].Lease.ReferenceUnixNS = fmt.Sprint(n - 4000000000)
			case "stale":
				n := in.Hosts[0].Tracking.Rows[last].UnixSeconds*1000000000 + 60000000001
				in.Hosts[0].Lease.MeasuredUnixNS = fmt.Sprint(n)
				in.Hosts[0].Lease.ReferenceUnixNS = fmt.Sprint(n)
			}
			_, err := Select(in, p)
			want := map[string]string{"leap": "clock-select-leap", "reference": "clock-select-break", "expired": "clock-lease-expired", "stale": "clock-select-stale"}[field]
			if err == nil || err.Error() != want {
				t.Fatalf("MUTATION: post-window-%s: got %v, want %s", field, err, want)
			}
		})
	}
	in, p := selectionFixture()
	n := in.Hosts[0].Tracking.Rows[len(in.Hosts[0].Tracking.Rows)-1].UnixSeconds*1000000000 + 60000000000
	in.Hosts[0].Lease.MeasuredUnixNS, in.Hosts[0].Lease.ReferenceUnixNS = fmt.Sprint(n), fmt.Sprint(n)
	selectGood(t, in, p)
}

func TestClockSelectLeaseFreshness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offset  int64
		refuses bool
	}{
		{"early", -1000000001, true}, {"lower-boundary", -1000000000, false},
		{"same-row", 0, false}, {"upper-boundary", 60000000000, false}, {"late", 60000000001, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, slot := range selectionSlots {
				in, p := selectionFixture()
				last := in.Hosts[i].Tracking.Rows[len(in.Hosts[i].Tracking.Rows)-1].UnixSeconds * 1000000000
				in.Hosts[i].Lease.MeasuredUnixNS, in.Hosts[i].Lease.ReferenceUnixNS = fmt.Sprint(last+tc.offset), fmt.Sprint(last+tc.offset)
				_, err := Select(in, p)
				if tc.refuses {
					if err == nil || err.Error() != "clock-select-stale" {
						t.Fatalf("MUTATION: lease-freshness-%s: slot %s: %v", tc.name, slot, err)
					}
				} else {
					require.NoError(t, err, "slot %s at inclusive freshness boundary", slot)
				}
			}
		})
	}
}

func TestClockSelectAddsFrontendError(t *testing.T) {
	in, p := selectionFixture()
	r := selectGood(t, in, p)
	if r.Receipt.Bounds.FrontendControlBoundNS != 826 || r.Receipt.Bounds.FrontendWorkerBoundNS != 883 {
		t.Fatal("MUTATION: omit-frontend", r.Receipt.Bounds)
	}
}

func TestClockSelectSignalReserveExactlyOnce(t *testing.T) {
	in, p := selectionFixture()
	r := selectGood(t, in, p)
	if r.Receipt.Bounds.FrontendWorkerBoundNS != 883 {
		t.Fatal("MUTATION: signal-reserve", r.Receipt.Bounds)
	}
}

func TestClockSelectMixedModesRefuse(t *testing.T) {
	in, p := selectionFixture()
	in.Hosts[0].LeapMode = "smeared"
	if _, err := Select(in, p); err == nil || err.Error() != "clock-select-leap" {
		t.Fatal("MUTATION: mixed-mode", err)
	}
}

func TestClockSelectLeapRefuses(t *testing.T) {
	for _, status := range []string{"+", "-", "?"} {
		in, p := selectionFixture()
		in.Hosts[0].Tracking.Rows[3].LeapStatus = status
		if _, err := Select(in, p); err == nil || err.Error() != "clock-select-leap" {
			t.Fatal("MUTATION: scheduled-leap", err)
		}
	}
}

func TestClockSelectColumn14Padding(t *testing.T) {
	for raw, expected := range map[string]string{"4.244e-02": "42445000", "1.234e-06": "2469/2", "9.999e-10": "19999/20000"} {
		r, err := Column14MaximumNS(raw)
		if err != nil || r.RatString() != expected {
			t.Fatal("MUTATION: missing-column14-pad", raw, r, err)
		}
	}
	for _, raw := range []string{"", "nan", "-1.000e-06", "1e-6", "1.000e-999", "1.000e+999"} {
		_, err := Column14MaximumNS(raw)
		require.Error(t, err)
	}
}

func TestClockSelectAdmissionAndTrailingWindow(t *testing.T) {
	changes := map[string]func(*SelectionEvidence){
		"backwards update": func(e *SelectionEvidence) {
			e.Hosts[0].Tracking.Rows[2], e.Hosts[0].Tracking.Rows[3] = e.Hosts[0].Tracking.Rows[3], e.Hosts[0].Tracking.Rows[2]
		},
		"source change":    func(e *SelectionEvidence) { e.Hosts[0].Tracking.Rows[3].Reference = "different-source" },
		"self consistency": func(e *SelectionEvidence) { e.Hosts[0].Tracking.Rows[3].OffsetSeconds = 1 },
		"old interval":     func(e *SelectionEvidence) { e.StartUnixSeconds--; e.EndUnixSeconds-- },
		"missing host":     func(e *SelectionEvidence) { e.Hosts = e.Hosts[:3] },
		"duplicate slot":   func(e *SelectionEvidence) { e.Hosts[1].Slot = "relay" },
		"partial read":     func(e *SelectionEvidence) { e.Hosts[0].Tracking.ReadComplete = false },
		"tail missing":     func(e *SelectionEvidence) { e.Hosts[0].Tracking.Rows = e.Hosts[0].Tracking.Rows[:120] },
		"gap": func(e *SelectionEvidence) {
			e.Hosts[0].Tracking.Rows = append(e.Hosts[0].Tracking.Rows[:3], e.Hosts[0].Tracking.Rows[100:]...)
		},
		"break": func(e *SelectionEvidence) {
			e.Hosts[0].Tracking.Breaks = []TrackingBreak{{Kind: "log-loss", FromUnixSeconds: e.StartUnixSeconds, UnixSeconds: e.EndUnixSeconds}}
		},
		"missing probes":       func(e *SelectionEvidence) { e.Falsification.Records = nil },
		"unchecked probes":     func(e *SelectionEvidence) { e.Falsification.Records[0].Status = "unchecked" },
		"probe loss":           func(e *SelectionEvidence) { e.Falsification.LossCounts.OmittedRecords = 1 },
		"sticky falsification": func(e *SelectionEvidence) { e.Falsification.CountsByStatus["falsified"] = 1 },
		"disjoint probe": func(e *SelectionEvidence) {
			e.Falsification.Records[0].HostReceive = e.Falsification.Records[0].FrontendSend
			e.Falsification.Records[0].HostSend = e.Falsification.Records[0].FrontendSend
			e.Falsification.Records[0].FrontendReceive = e.Falsification.Records[0].FrontendSend
			e.Falsification.Records[0].HostReceive = fmt.Sprint(e.StartUnixSeconds*1e9 + 100000)
			e.Falsification.Records[0].HostSend = e.Falsification.Records[0].HostReceive
		},
		"excess growth": func(e *SelectionEvidence) { e.Hosts[0].Lease.DispersionEndSeconds = "0.001" },
		"no lease":      func(e *SelectionEvidence) { e.Hosts[0].Lease.Available = false },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			in, p := selectionFixture()
			change(&in)
			_, err := Select(in, p)
			require.Error(t, err)
		})
	}
	in, p := selectionFixture()
	p.WindowSeconds = 119
	_, err := Select(in, p)
	require.Error(t, err)
}

func TestClockSelectIncludesTouchingPredecessor(t *testing.T) {
	in, p := selectionFixture()
	in.Hosts[0].Tracking.Rows = in.Hosts[0].Tracking.Rows[1:]
	in.Hosts[0].Tracking.Rows[0].MaxErrorDecimal = "2.000e-06"
	in.Hosts[0].Tracking.Rows[0].MaxErrorSeconds = 2e-6
	r := selectGood(t, in, p)
	require.Equal(t, "4001/2", r.Receipt.Hosts[0].MaximumNS)
}

func TestClockSelectWarmupUncheckedDoesNotEraseStickyFalsification(t *testing.T) {
	in, p := selectionFixture()
	warmup := ProbeRecord{Target: "control", FrontendReceive: fmt.Sprint((in.StartUnixSeconds - 1) * 1000000000), Status: "unchecked"}
	in.Falsification.Records = append(in.Falsification.Records, warmup)
	selectGood(t, in, p)
	in.Falsification.Records[len(in.Falsification.Records)-1].Status = "falsified"
	_, err := Select(in, p)
	require.Error(t, err)
	in.Falsification.Records[len(in.Falsification.Records)-1].Status = "unchecked"
	in.Falsification.Records[len(in.Falsification.Records)-1].FrontendReceive = "unavailable"
	_, err = Select(in, p)
	require.Error(t, err)
}

func TestClockSelectReceiptDeterminismAndEvidenceBinding(t *testing.T) {
	in, p := selectionFixture()
	before, _ := json.Marshal(in)
	a := selectGood(t, in, p)
	for range 3 {
		b := selectGood(t, in, p)
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		require.Equal(t, ab, bb)
	}
	after, _ := json.Marshal(in)
	require.Equal(t, before, after, "selection mutated input")
	slices.Reverse(in.Hosts)
	b := selectGood(t, in, p)
	require.Equal(t, a, b, "host input order changed digest")
	in.Hosts[0].Tracking.Rows[0].RootDelaySeconds = 0.01
	b = selectGood(t, in, p)
	require.NotEqual(t, a.SHA256, b.SHA256)
	require.NotEqual(t, a.Receipt.EvidenceSHA256, b.Receipt.EvidenceSHA256)
	require.Equal(t, a.Receipt.Bounds, b.Receipt.Bounds)
	raw, _ := json.Marshal(a)
	require.NotContains(t, string(raw), "referenceUnixNs")
}

func TestRecordedLeaseAndOptionalParsingAreVerdictNeutral(t *testing.T) {
	o := leaseFixture()
	l, err := RecordLease(o, 1e9)
	require.NoError(t, err)
	require.Equal(t, "1/500", l.GrowthPPM)
	require.Equal(t, int64(1002), l.BoundNS)
	require.Equal(t, int64(4e9), l.RemainingNS)
	l, err = RecordLease(o, 5e9)
	require.NoError(t, err)
	require.True(t, l.Expired)
	_, err = RecordLease(o, 1)
	require.Error(t, err)
	const baseline = `{"errorBoundNs":1,"rateErrorPpm":0.1,"measuredUnixNs":"1792886400000000000"}`
	base, err := Parse([]byte(baseline))
	require.NoError(t, err)
	lease, _ := json.Marshal(o)
	for _, extension := range []string{string(lease), `null`, `42`, `{"available":true,"available":false}`, `{"available":false,"reason":"private-label"}`} {
		raw := strings.TrimSuffix(baseline, "}") + `,"lease":` + extension + `}`
		parsed, err := Parse([]byte(raw))
		require.NoError(t, err)
		pub := PublicSample(parsed)
		pub.Lease, pub.RecordedLease = nil, nil
		a, _ := json.Marshal(base)
		b, _ := json.Marshal(pub)
		require.True(t, bytes.Equal(a, b), "%s != %s", a, b)
		rows := map[string]Sample{"relay": parsed, "shared": parsed, "w1": parsed, "w2": parsed}
		e, err := Check(rows, Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 1}, "relay", "shared")
		require.NoError(t, err)
		require.Equal(t, int64(2), e.Control)
	}
	private := Sample{TrackingLog: &TrackingLog{Rows: []TrackingRow{{MaxErrorDecimal: "private-label"}}}}
	public := PublicSample(private)
	raw, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-label")
	require.Equal(t, "private-label", private.TrackingLog.Rows[0].MaxErrorDecimal)
}
