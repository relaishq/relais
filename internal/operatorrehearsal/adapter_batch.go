package operatorrehearsal

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// Five maximum packed host replies need about 121 KB. Keep more than twice
// that payload room, matching observe.Decode's strict 256000-byte limit and
// leaving frame metadata room. External request arguments retain 128 KiB.
const hostBatchReplyLimit = 256000

// Validate every read before submission. Inventory remains serial to avoid
// disturbing the media baseline; fixed lease observations run concurrently.
// Join every child, preserving its certainty and original deadline.
func (a *Adapter) executeScriptReadBatch(ctx context.Context, req operator.Request) operator.Reply {
	var args operator.HostReadBatchArgs
	if len(req.Args) > operator.MaxFrameBytes/2 || observe.Decode(req.Args, &args, observe.Contract{"reads"}) != nil {
		return refusal(req.ID, "invalid-request")
	}
	return a.executeReadBatch(ctx, req, args.Reads)
}

// Lease scripts are generated locally from validated compact inputs. Do not
// serialize those scripts into a second wire request: five exceed its limit.
func (a *Adapter) executeReadBatch(ctx context.Context, req operator.Request, reads []operator.HostRead) operator.Reply {
	if a.Topology == nil || a.Runner == nil {
		return refusal(req.ID, "invalid-request")
	}
	slots := a.Topology.Slots()
	if req.Target != "hosts" || (req.Action != "inventory" && req.Action != "clock") || len(req.Script) != 0 || len(reads) != len(slots) {
		return refusal(req.ID, "invalid-request")
	}
	for i, read := range reads {
		if read.Target != slots[i] || len(read.Script) == 0 || len(read.Script) > operator.MaxScriptBytes {
			return refusal(req.ID, "invalid-request")
		}
		if _, ok := a.Topology.Hosts[read.Target]; !ok || !dockerID.MatchString(a.Containers[read.Target]) {
			return refusal(req.ID, "invalid-target")
		}
	}
	deadline, _ := ctx.Deadline()
	if ctx.Err() != nil || req.Operation == operator.HostReadBatch && time.Until(deadline) < operator.HostSubmissionMinimum {
		return refusal(req.ID, "deadline-before-submission")
	}
	out := operator.HostReadBatchOutput{Replies: make([]operator.Reply, len(slots))}
	run := func(i int, read operator.HostRead) {
		child := operator.Request{ID: read.Target, Operation: req.Operation, Target: read.Target, Action: req.Action, Script: read.Script}
		out.Replies[i] = a.executeHost(ctx, child)
	}
	var pending sync.WaitGroup
	for i, read := range reads {
		if req.Operation == operator.HostLeaseReadBatch {
			pending.Add(1)
			go func() { defer pending.Done(); run(i, read) }()
		} else {
			run(i, read)
		}
	}
	pending.Wait()
	payload, err := json.Marshal(out)
	if err != nil || len(payload) > hostBatchReplyLimit {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown, UncertaintyRule: "truncated-output"}
	}
	zero := 0
	return operator.Reply{ID: req.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: payload}
}
