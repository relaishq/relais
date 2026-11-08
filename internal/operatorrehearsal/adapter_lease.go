package operatorrehearsal

import (
	"context"
	"encoding/json"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
)

func (a *Adapter) executeLeaseRead(ctx context.Context, req operator.Request) operator.Reply {
	reads, err := host.LeaseReadScripts(req)
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
