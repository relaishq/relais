package clocks

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/stretchr/testify/require"
)

func boundCoverageFixture(t *testing.T) CoverageEvidence {
	t.Helper()
	raw, err := os.ReadFile("testdata/bound-receipt.json")
	require.NoError(t, err)
	var s ClockSelection
	require.NoError(t, json.Unmarshal(raw, &s))
	start, end := "1793930010250000000", "1793930020250000000"
	e := CoverageEvidence{FrontendSlot: "relay", ControlSlot: "shared", Windows: []observe.CoverageWindow{{Kind: "takeover", Worker: "w1", StartUnixNS: start, EndUnixNS: end}}, KillBrackets: []observe.CoverageBracket{{Worker: "w1", MidpointUnixNS: start, BeforeUnixNS: "1793930010249999999", AfterUnixNS: "1793930010250000001"}}, Attempts: []CoverageAttempt{{Version: 2, Clocks: map[string]observe.CoverageTimeline{}}}}
	for _, slot := range selectionSlots {
		log := observe.CoverageTimeline{ReadComplete: true}
		for at := int64(1793930000); at <= 1793930035; at++ {
			log.Rows = append(log.Rows, observe.CoverageRow{UnixSeconds: at, LeapStatus: "N"})
		}
		e.Attempts[0].Clocks[slot] = log
		boundError(&e, slot, "5.000e-05")
	}
	boundError(&e, "shared", "1.000e-06")
	e, err = e.WithBinding(ReceiptBinding{ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, Bounds: s.Receipt.Bounds}, s.Receipt)
	require.NoError(t, err)
	return e
}
func boundError(e *CoverageEvidence, slot, raw string) {
	n, _ := strconv.ParseFloat(raw, 64)
	for i := range e.Attempts[0].Clocks[slot].Rows {
		r := &e.Attempts[0].Clocks[slot].Rows[i]
		r.MaxErrorSeconds, r.MaxErrorDecimal = n, raw
	}
}
func TestBoundCoverage(t *testing.T) {
	for _, name := range []string{"fixed-source", "pad", "omit-K", "K-once", "rule4", "rule4-no-K", "stripped", "altered", "token-private"} {
		t.Run(name, func(t *testing.T) {
			e := boundCoverageFixture(t)
			want := ""
			switch name {
			case "token-private":
				e.Attempts[0].Clocks["relay"].Rows[10].MaxErrorDecimal = "private-source-token"
				want = "clock-coverage-missing"
			case "fixed-source":
				e.Windows[0].Kind = "move"
				boundError(&e, "shared", "8.000e-05")
				want = "clock-coverage-bound"
			case "pad", "omit-K":
				boundError(&e, "relay", "6.000e-05")
				boundError(&e, "w1", "6.000e-05")
				want = "clock-coverage-bound"
			case "rule4":
				e.KillBrackets[0].BeforeUnixNS = "1793930010249900000"
				e.KillBrackets[0].AfterUnixNS = "1793930010250100000"
				want = "clock-coverage-kill-bracket"
			case "rule4-no-K":
				e.KillBrackets[0].BeforeUnixNS = "1793930010249950000"
				e.KillBrackets[0].AfterUnixNS = "1793930010250050000"
			case "stripped":
				e.Binding = nil
				want = "clock-coverage-binding"
			case "altered":
				e.Binding.Receipt.Policy.SignalReserveNS++
				want = "clock-coverage-binding"
			}
			raw, err := json.Marshal(e)
			require.NoError(t, err)
			if strings.Contains(string(raw), "private-source-token") {
				t.Fatal("MUTATION: bound-token-private")
			}
			var replay CoverageEvidence
			require.NoError(t, json.Unmarshal(raw, &replay))
			replay.Evaluate()
			causes := replay.Results[0].Causes
			if want == "" && len(causes) != 0 || want != "" && !slices.Contains(causes, want) {
				t.Fatalf("MUTATION: bound-%s: %v", name, causes)
			}
			if name == "stripped" && replay.DrainComplete {
				t.Fatal("MUTATION: bound-stripped")
			}
		})
	}
}
func TestBoundCoverageConsumers(t *testing.T) {
	e := boundCoverageFixture(t)
	rows := map[string]Sample{}
	for _, slot := range selectionSlots {
		rows[slot] = Sample{ErrorBoundNS: 100000}
	}
	if _, err := e.Check(rows); err == nil {
		t.Fatal("MUTATION: bound-check")
	}
	if _, err := e.SignalMidpoint("1793930010000000000", "1793930010000200000", 80000); err == nil {
		t.Fatal("MUTATION: bound-signal")
	}
	e.Windows[0].Kind = "move"
	e.Windows[0].StartUnixNS = "1793930010999500000"
	e.Windows[0].EndUnixNS = e.Windows[0].StartUnixNS
	for slot, log := range e.Attempts[0].Clocks {
		log.Rows = log.Rows[:13]
		e.Attempts[0].Clocks[slot] = log
	}
	e.Evaluate()
	if !e.DrainComplete {
		t.Fatal("MUTATION: bound-drain")
	}
	q := e.Qualification()
	if len(q.WindowCausesForReceipt(0, 0, true, e.ClockReceiptSHA256)) == 0 {
		t.Fatal("MUTATION: bound-precoverage")
	}
}

func TestBoundCheckReserve(t *testing.T) {
	e := boundCoverageFixture(t)
	rows := map[string]Sample{}
	for _, slot := range selectionSlots {
		rows[slot] = Sample{ErrorBoundNS: 30000}
	}
	if _, err := e.Check(rows); err != nil {
		t.Fatal("MUTATION: bound-check-K", "in-bounds", err)
	}
	rows["w1"] = Sample{ErrorBoundNS: 100000}
	if _, err := e.Check(rows); err == nil {
		t.Fatal("MUTATION: bound-check-K", "worker reserve omitted")
	}
}

func TestBoundCoverageRebind(t *testing.T) {
	for _, digest := range []string{"same", strings.Repeat("0", 64)} {
		e := boundCoverageFixture(t)
		if digest != "same" {
			e.ClockReceiptSHA256 = digest
		}
		if _, err := e.WithBinding(e.Binding.Binding, e.Binding.Receipt); err == nil {
			t.Fatal("MUTATION: bound-rebind", digest)
		}
	}
}
