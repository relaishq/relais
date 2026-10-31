package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

func TestSequenceDiagnosticsPreserveRelayTimingExemption(t *testing.T) {
	report := passingReport()
	clock := &operatorgates.FakeClock{Now: time.UnixMilli(1000000)}
	session, err := operatorgates.NewSession("relay-diagnostics", operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000"}, clock)
	if err != nil {
		t.Fatal(err)
	}
	sequence := session.RunSequence(context.Background(), operatorgates.SequencePorts{Command: func(_ context.Context, gate operator.Gate, _ time.Duration) operatorgates.ProcedureResult {
		for _, row := range report.Gates {
			if row.Gate == gate {
				p := row.ProcedureResult
				p.Completed = true
				return p
			}
		}
		t.Fatal("unexpected gate")
		return operatorgates.ProcedureResult{}
	}, Finalize: func(context.Context, time.Duration) (*operatorgates.MediaSummary, error) {
		return &operatorgates.MediaSummary{Status: operator.Passed}, nil
	}})
	r := &rehearsal{report: &report}
	r.recordSequence(sequence, nil)
	report.Evaluate()
	if report.Verdict != operator.Passed || len(report.Exemptions) != 2 {
		t.Fatal("MUTATION: additive diagnostics broke rehearsal relay exemption", report.Failures)
	}
	var data bytes.Buffer
	if err = report.Write(&data); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err = json.Unmarshal(data.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Verdict != operator.Passed || saved.Gates[3].Reason != "" || saved.Gates[3].Status != operator.Inconclusive {
		t.Fatal("MUTATION: relay reason changed in serialized report")
	}
}
