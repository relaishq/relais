package operatorrehearsal

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
)

func (a *Adapter) executeLeaseRead(ctx context.Context, req operator.Request) (reply operator.Reply) {
	started := time.Now()
	deadline, _ := ctx.Deadline()
	budget := time.Until(deadline).Milliseconds()
	defer func() {
		if a.Diagnostics != nil {
			id, _ := strconv.ParseUint(req.ID, 10, 64)
			a.mu.Lock()
			defer a.mu.Unlock()
			_, _ = fmt.Fprintf(a.Diagnostics, "clock-lease-read operation=%s requestId=%d durationMs=%d budgetMs=%d certainty=%s\n", req.Operation, id, time.Since(started).Milliseconds(), budget, reply.Transport)
		}
	}()
	reads, err := host.LeaseReadScripts(req, a.LeaseProbeFiles)
	if err != nil {
		return refusal(req.ID, "invalid-request")
	}
	if req.Operation == operator.HostLeaseReadBatch {
		req.Args, _ = json.Marshal(operator.HostReadBatchArgs{Reads: reads})
		return a.executeReadBatch(ctx, req)
	}
	req.Script = reads[0].Script
	return a.executeHost(ctx, req)
}
