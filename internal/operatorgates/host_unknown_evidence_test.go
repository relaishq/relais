package operatorgates

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/relaishq/relais/pkg/operator"
)

func TestHostExceptionCertaintyUnchanged(t *testing.T) {
	for _, action := range []string{"status", "worker-status"} {
		t.Run(action, func(t *testing.T) {
			w, p := newRoundTripWorld(t, false)
			calls := 0
			w.runtime.Peer.(*observationPeer).respond = func(req operator.Request) operator.Reply {
				calls++
				if action == "status" {
					return hostReply(t, []byte(`{"kind":"unknown"}`))
				}
				return hostReply(t, []byte(`{"kind":"failed","code":"timeout-error"}`))
			}
			result, err := p.Step(context.Background(), action, "shared", GateMaximum, ProcessExtra{Worker: "w1"})
			if action == "worker-status" {
				if code(err) != "precondition-failed:timeout-error" || result.Certainty != operator.Failed || w.runtime.Session.Latched() {
					t.Fatalf("failed pure read changed certainty: %+v %v", result, err)
				}
				return
			}
			if code(err) != "command-result-unknown" || result.Certainty != operator.Unknown || !w.runtime.Session.Latched() || len(w.runtime.Session.Final(nil).Uncertainties) != 1 {
				t.Fatalf("effectful exception did not latch unknown: %+v %v", result, err)
			}
			_, _ = p.Step(context.Background(), action, "shared", GateMaximum, ProcessExtra{})
			if calls != 1 {
				t.Fatalf("unknown effect was retried: %d requests", calls)
			}
		})
	}
}

func TestUnknownHostEvidenceReachesReport(t *testing.T) {
	for _, phase := range []string{"status", "post"} {
		t.Run(phase, func(t *testing.T) {
			w, p := newRoundTripWorld(t, false)
			peer := w.runtime.Peer.(*observationPeer)
			world := peer.respond
			moves := 0
			peer.respond = func(req operator.Request) operator.Reply {
				if req.Action != "move" {
					return world(req)
				}
				moves++
				return hostReply(t, []byte(`{"kind":"unknown","code":"timeout-error","elapsedMs":5150,"phase":"`+phase+`"}`))
			}
			_, err := p.Step(context.Background(), "move", "shared", GateMaximum, ProcessExtra{Worker: "w2", Session: "original", Owner: "w1", Epoch: 1})
			if code(err) != "command-result-unknown" || !w.runtime.Session.Latched() || moves != 1 {
				t.Fatalf("unknown move behavior changed: %v, moves=%d", err, moves)
			}
			raw, _ := json.Marshal(w.runtime.Session.Final(nil))
			var report struct {
				Uncertainties []map[string]any `json:"uncertainties"`
			}
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Uncertainties) != 1 || report.Uncertainties[0]["code"] != "timeout-error" || report.Uncertainties[0]["elapsedMs"] != float64(5150) || report.Uncertainties[0]["phase"] != phase {
				t.Fatalf("host failure evidence absent from report: %s", raw)
			}
		})
	}
}
