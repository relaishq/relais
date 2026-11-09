package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

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
