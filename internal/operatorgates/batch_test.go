package operatorgates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// The optional operation uses the existing terminal certainty and latch path.
// No fallback is allowed after a submitted batch has an unknown result.
func TestBatchedProofFailureAndUncertainty(t *testing.T) {
	for _, kind := range []string{"ok", "unknown-first", "unknown-last", "bad-output", "duplicate", "missing", "failed-and-unknown", "failed", "refused", "outer-refused", "outer-failed-unknown", "outer-failed-no-exit", "outer-failed-unreadable", "outer-failed-definite", "outer-nonzero-unknown", "outer-nonzero-definite", "child-failed-no-exit", "child-failed-no-payload", "late", "floor"} {
		t.Run(kind, func(t *testing.T) {
			r, peer, _ := runtimeFixture(t)
			r.Session.capabilities[operator.HostReadBatch] = true
			original := peer.respond
			peer.respond = func(req operator.Request) operator.Reply {
				if req.Operation != operator.HostReadBatch {
					return original(req)
				}
				var args operator.HostReadBatchArgs
				if err := json.Unmarshal(req.Args, &args); err != nil {
					t.Fatal(err)
				}
				out := operator.HostReadBatchOutput{}
				for _, read := range args.Reads {
					child := original(operator.Request{Action: req.Action, Target: read.Target})
					child.ID = read.Target
					out.Replies = append(out.Replies, child)
				}
				switch kind {
				case "unknown-first":
					out.Replies[0].Transport = operator.Unknown
				case "unknown-last":
					out.Replies[3].Transport = operator.Unknown
				case "bad-output":
					out.Replies[3].Payload = json.RawMessage(`{}`)
				case "duplicate":
					out.Replies[3].ID = out.Replies[0].ID
				case "missing":
					out.Replies = out.Replies[:3]
				case "failed-and-unknown":
					out.Replies[0].Transport = operator.Failed
					out.Replies[3].Transport = operator.Unknown
				case "outer-failed-unknown", "outer-failed-no-exit", "outer-nonzero-unknown":
					out.Replies[0].Transport = operator.Failed
					out.Replies[3].Transport = operator.Unknown
				case "outer-failed-definite":
					out.Replies[0].Transport = operator.Failed
					one := 1
					out.Replies[0].ExitCode = &one
				case "child-failed-no-exit":
					out.Replies[0].Transport = operator.Failed
					out.Replies[0].ExitCode = nil
				case "child-failed-no-payload":
					out.Replies[0].Transport = operator.Failed
					out.Replies[0].Payload = nil
				case "failed":
					out.Replies[0].Transport = operator.Failed
				case "refused":
					out.Replies[3] = operator.Reply{ID: "shared", Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
				case "late":
					r.Session.clock.(*FakeClock).Advance(2 * time.Minute)
				}
				zero := 0
				reply := operator.Reply{Transport: operator.Succeeded, ExitCode: &zero, Payload: oracleJSON(out)}
				switch kind {
				case "outer-refused":
					reply = operator.Reply{Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
				case "outer-failed-unknown", "outer-failed-no-exit", "outer-failed-unreadable", "outer-failed-definite":
					reply.Transport = operator.Failed
					one := 1
					reply.ExitCode = &one
				case "outer-nonzero-unknown", "outer-nonzero-definite":
					one := 1
					reply.ExitCode = &one
				}
				if kind == "outer-failed-no-exit" {
					reply.ExitCode = nil
				}
				if kind == "outer-failed-unreadable" {
					reply.Payload = nil
				}
				return reply
			}
			end := time.Minute
			if kind == "floor" {
				end = operator.HostSubmissionMinimum - time.Millisecond
			}
			_, err := r.Step(context.Background(), "freeze", "shared", end, ProcessExtra{})
			unknown := kind != "ok" && kind != "failed" && kind != "refused" && kind != "floor" && kind != "outer-refused" && kind != "outer-failed-definite" && kind != "outer-nonzero-definite"
			if r.Session.Latched() != unknown || (err == nil) != (kind == "ok") {
				t.Fatalf("MUTATION: batch-certainty %s err=%v latched=%t", kind, err, r.Session.Latched())
			}
			if kind == "outer-failed-definite" || kind == "outer-nonzero-definite" {
				traces := r.Session.trace
				if traces[len(traces)-1].Certainty != operator.Failed {
					t.Fatal("MUTATION: batch-definite-read-outcome")
				}
			}
			sent := peer.sent
			if kind == "floor" && sent != 0 {
				t.Fatal("MUTATION: batch-floor")
			}
			if unknown {
				_, _ = r.Step(context.Background(), "unfreeze", "shared", time.Minute, ProcessExtra{})
				if peer.sent != sent {
					t.Fatal("MUTATION: batch-unknown-replay")
				}
			}
			if kind != "ok" {
				for _, tr := range r.Session.trace {
					if tr.Event == "request" && tr.Operation == operator.HostExecute {
						t.Fatal("MUTATION: batch-failure-mutates")
					}
				}
			} else if sent != 3 {
				t.Fatalf("MUTATION: batch-count got %d want 3", sent)
			}
		})
	}
}
