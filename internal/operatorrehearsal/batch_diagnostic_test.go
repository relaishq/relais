package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

func TestBatchObservationRetainsHostReasonAndDiagnostic(t *testing.T) {
	for _, kind := range []string{"timeout", "truncated-output", "unsafe-reason", "outer-refused"} {
		t.Run(kind, func(t *testing.T) {
			clock := &operatorgates.FakeClock{Now: time.UnixMilli(1000000)}
			init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000", Capabilities: []operator.Operation{operator.HostReadBatch}}
			session, err := operatorgates.NewSession("idle", init, clock)
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			rehearsal := &rehearsal{diagnostics: &log}
			session.SetObservationFailureSink(func(slot, action string, err error) {
				if err.Error() == "command-result-unknown" && !session.Latched() {
					t.Fatal("MUTATION: batch-diagnostic-before-latch")
				}
				rehearsal.preconditionDiagnostic(slot, action, err)
			})
			peer := &idlePeer{respond: func(operator.Request) operator.Reply {
				if kind == "outer-refused" {
					return refusal("1", "deadline-before-submission")
				}
				out := operator.HostReadBatchOutput{}
				for _, slot := range Slots {
					out.Replies = append(out.Replies, operator.Reply{ID: slot, Transport: operator.Succeeded, ExitCode: code(0), Payload: json.RawMessage(`{"stdout":""}`)})
				}
				out.Replies[1].Transport = operator.Unknown
				out.Replies[1].UncertaintyRule = kind
				if kind == "unsafe-reason" {
					out.Replies[1].UncertaintyRule = "unsafe-value"
				}
				raw, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				return operator.Reply{Transport: operator.Succeeded, ExitCode: code(0), Payload: raw}
			}}
			_, err = session.FreshClocks(context.Background(), peer, clocks.Bounds{}, time.Minute)
			if err == nil {
				t.Fatal("batch failure became proof")
			}
			expected := "precondition-check host=w1 observation=clock category=command-result-unknown\n"
			if kind == "outer-refused" {
				expected = "precondition-check host=unavailable observation=clock category=precondition-failed\n"
			}
			if log.String() != expected {
				t.Fatalf("MUTATION: batch-observation-diagnostic got %q want %q", log.String(), expected)
			}
			if kind == "outer-refused" {
				if session.Latched() || len(session.Uncertainties()) != 0 {
					t.Fatal("proved refusal became unknown")
				}
				return
			}
			reason := kind
			if kind == "unsafe-reason" {
				reason = "operation-evidence-unknown"
			}
			found := false
			for _, u := range session.Uncertainties() {
				if u.Kind == "host.execute" && u.Target == "w1" && u.Action == "clock" && u.Rule == reason && u.RequestID == "1" && u.DeadlineUnixMS == "1060000" {
					found = true
				}
			}
			if !found || !session.Latched() {
				t.Fatalf("MUTATION: batch-child-detail %v", session.Uncertainties())
			}
		})
	}
}
