package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func TestBoundOffline(t *testing.T) {
	raw, err := os.ReadFile("../../../internal/operatorgates/clocks/testdata/bound-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	var s clocks.ClockSelection
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	e, err := (clocks.CoverageEvidence{FrontendSlot: "relay", ControlSlot: "shared", Windows: []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}}}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, Bounds: s.Receipt.Bounds}, s.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"bound", "stripped", "historical", "pool-bound-coverage-stripped"} {
		report := storedReport{ClockReceiptSHA256: s.SHA256, Bounds: s.Receipt.Bounds, Coverage: &e}
		args := []string{}
		if mode == "pool-bound-coverage-stripped" {
			stripped := e
			stripped.ClockReceiptSHA256, stripped.Binding, stripped.Bounds = "", nil, clocks.FixedCoverageBounds()
			report.ClockReceiptSHA256, report.Bounds, report.Coverage = "", stripped.Bounds, &stripped
			pool, _ := json.Marshal(operatorevidence.Pool{RunID: s.Receipt.RunID, ClockReceiptSHA256: s.SHA256})
			path := filepath.Join(t.TempDir(), "pool.json")
			if err := os.WriteFile(path, pool, 0600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "-pool", path)
		}
		if mode == "stripped" {
			report.ClockReceiptSHA256 = ""
		}
		if mode == "historical" {
			report.Coverage = nil
		}
		data, _ := json.Marshal(report)
		path := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := evaluate(append(args, "-report", path), &out)
		if (mode == "bound") != (err == nil) {
			t.Fatal("MUTATION: bound-offline", mode, err)
		}
	}
}
