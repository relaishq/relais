package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

func TestBindingRefusalPublicCause(t *testing.T) {
	report := passingReport()
	var diagnostics bytes.Buffer
	r := &rehearsal{report: &report, diagnostics: &diagnostics}
	r.recordError("clock-binding-failed", &clocks.BindingError{Cause: "clock-history-incomplete"})
	var raw bytes.Buffer
	if err := report.Write(&raw); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(raw.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Errors) != 2 || saved.Errors[0] != "clock-binding-failed" || saved.Errors[1] != "clock-history-incomplete" || diagnostics.String() != "clock-binding-failed: clock-history-incomplete\n" {
		t.Fatal("binding refusal lost", saved.Errors, diagnostics.String())
	}
	for _, code := range []string{"clock-binding-failed", "clock-history-incomplete"} {
		if publicCode(code+":unsafe-detail") != "unclassified" {
			t.Fatal("binding refusal disclosed arbitrary detail")
		}
	}
}

func TestLeaseRefusalPublicCause(t *testing.T) {
	for _, cause := range []string{"clock-lease-missing", "clock-lease-expired", "clock-lease-bound", "clock-config-differ"} {
		t.Run(cause, func(t *testing.T) {
			r := passingReport()
			r.Gates[0].Status = operator.Inconclusive
			r.Gates[0].Reason = cause
			r.HeadCaller = &HeadCallerReport{Gates: []GateReport{r.Gates[0]}}
			var raw bytes.Buffer
			if err := r.Write(&raw); err != nil {
				t.Fatal(err)
			}
			var got Report
			if err := json.Unmarshal(raw.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Gates[0].Reason != cause || got.HeadCaller.Gates[0].Reason != cause || got.Gates[0].Status != operator.Inconclusive {
				t.Fatal("MUTATION: lease-refusal-public-cause", got.Gates[0].Reason, got.HeadCaller.Gates[0].Reason)
			}
			var diagnostics bytes.Buffer
			rehearsal := &rehearsal{diagnostics: &diagnostics}
			rehearsal.preconditionDiagnostic("hosts", "clock", &operatorgates.ProcedureError{Code: cause})
			want := "precondition-check host=unavailable observation=clock category=" + cause + "\n"
			if diagnostics.String() != want {
				t.Fatal("MUTATION: lease-refusal-public-cause", diagnostics.String())
			}
			if publicCode(cause+":unsafe-detail") != "unclassified" {
				t.Fatal("MUTATION: lease-refusal-public-cause unsafe suffix")
			}
		})
	}
}
