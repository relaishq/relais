package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/callharness"
)

func boundPoolFixture(t *testing.T) (previewPoolInput, *clocks.CoverageEvidence) {
	t.Helper()
	raw, err := os.ReadFile("../../operatorgates/clocks/testdata/bound-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	var s clocks.ClockSelection
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	e := coverageEvidenceFixture(t)
	for _, log := range e.Attempts[0].Clocks {
		for i := range log.Rows {
			log.Rows[i].MaxErrorSeconds, log.Rows[i].MaxErrorDecimal = .000001, "1.000e-06"
		}
	}
	bound, err := e.WithBinding(clocks.ReceiptBinding{ReceiptSHA256: s.SHA256, EvidenceSHA256: s.Receipt.EvidenceSHA256, Bounds: s.Receipt.Bounds}, s.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	input := rawPoolSample()
	input.PreCoverageEvidence = false
	input.ClockReceiptSHA256 = s.SHA256
	q := bound.Qualification()
	input.ClockCoverage = &q
	return input, &bound
}

func TestBoundJudgeRunID(t *testing.T) {
	input, e := boundPoolFixture(t)
	e.Binding.Receipt.RunID = "other-run"
	raw, _ := json.Marshal(e.Binding.Receipt)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	e.ClockReceiptSHA256, e.Binding.Binding.ReceiptSHA256 = digest, digest
	input.ClockReceiptSHA256 = digest
	q := e.Qualification()
	input.ClockCoverage = &q
	raw, _ = json.Marshal(input)
	var pool operatorevidence.Pool
	if err := json.Unmarshal(raw, &pool); err != nil {
		t.Fatal(err)
	}
	pool.ClockCoverage = e
	if _, err := operatorevidence.MarshalJudgeInput(pool); err == nil {
		t.Fatal("MUTATION: bound-run-wrapper")
	}
	decoded, err := decodePool(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := pooledPreviewMedia(&decoded); got.Status == "passed" {
		t.Fatal("MUTATION: bound-run-decoder", got)
	}
}
func TestBoundJudge(t *testing.T) {
	input, e := boundPoolFixture(t)
	raw, _ := json.Marshal(input)
	var pool operatorevidence.Pool
	if err := json.Unmarshal(raw, &pool); err != nil {
		t.Fatal(err)
	}
	pool.ClockCoverage = e
	wire, err := operatorevidence.MarshalJudgeInput(pool)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePool(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := pooledPreviewMedia(&decoded); got.Status != "passed" {
		t.Fatal("MUTATION: bound-judge", got)
	}
	pool.PreCoverageEvidence = true
	if _, err := operatorevidence.MarshalJudgeInput(pool); err == nil {
		t.Fatal("MUTATION: bound-precoverage-wrapper")
	}
	input.PreCoverageEvidence = true
	raw, _ = json.Marshal(input)
	if _, err := decodePool(raw); err == nil {
		t.Fatal("MUTATION: bound-precoverage-decoder")
	}
	input.ClockReceiptSHA256 = ""
	input.ClockCoverage.Binding = nil
	raw, _ = json.Marshal(input)
	if _, err := decodePool(raw); err == nil {
		t.Fatal("MUTATION: bound-precoverage-decoder")
	}
	input.ClockReceiptSHA256 = e.ClockReceiptSHA256
	input.PreCoverageEvidence = false
	input.ClockCoverage.Binding = nil
	if got := pooledPreviewMedia(&input); got.Status == "passed" {
		t.Fatal("MUTATION: bound-judge-stripped")
	}
	editPoolCalls(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("bound coverage hid an independent failure", got)
	}
}
