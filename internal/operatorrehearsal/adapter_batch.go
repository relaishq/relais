package operatorrehearsal

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// Validate the entire fixed read set before starting any child. The four host
// commands share the original deadline. All children are joined, without retry.
func (a *Adapter) executeReadBatch(ctx context.Context, req operator.Request) operator.Reply {
	var args operator.HostReadBatchArgs
	if req.Target != "hosts" || (req.Action != "inventory" && req.Action != "clock") || len(req.Script) != 0 || len(req.Args) > operator.MaxFrameBytes/2 || observe.Decode(req.Args, &args, observe.Contract{"reads"}) != nil || len(args.Reads) != len(Slots) || a.Topology == nil || a.Runner == nil {
		return refusal(req.ID, "invalid-request")
	}
	for i, read := range args.Reads {
		if read.Target != Slots[i] || len(read.Script) == 0 || len(read.Script) > operator.MaxScriptBytes {
			return refusal(req.ID, "invalid-request")
		}
		if _, ok := a.Topology.Hosts[read.Target]; !ok || !dockerID.MatchString(a.Containers[read.Target]) {
			return refusal(req.ID, "invalid-target")
		}
	}
	deadline, _ := ctx.Deadline()
	if ctx.Err() != nil || time.Until(deadline) < operator.HostSubmissionMinimum {
		return refusal(req.ID, "deadline-before-submission")
	}
	out := operator.HostReadBatchOutput{Replies: make([]operator.Reply, len(Slots))}
	var joined sync.WaitGroup
	for i, read := range args.Reads {
		joined.Go(func() {
			child := operator.Request{ID: read.Target, Operation: operator.HostReadBatch, Target: read.Target, Action: req.Action, Script: read.Script}
			out.Replies[i] = a.executeHost(ctx, child)
		})
	}
	joined.Wait()
	payload, err := json.Marshal(out)
	if err != nil || len(payload) > operator.MaxFrameBytes/2 {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown, UncertaintyRule: "truncated-output"}
	}
	zero := 0
	return operator.Reply{ID: req.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: payload}
}
