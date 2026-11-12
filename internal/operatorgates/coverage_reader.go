package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// CoveragePeer is a second adapter connection, used only for coverage reads.
type CoveragePeer interface {
	Send(context.Context, operator.Frame) error
	Receive(context.Context) (operator.Frame, error)
}

// CoverageSession labels requests on the coverage peer. They never share the
// gate session's identity, request numbers or latch.
const CoverageSession = "clock-coverage"

// NewCoverageReader reads trailing clock coverage through its own peer. Each
// read is one host.lease-read request: the adapter generates the fixed reader,
// and the short lease-read deadline applies instead of the 30 s host.execute
// floor. A lost, late or malformed reply closes this reader only: later reads
// fail and the tail stays incomplete.
func NewCoverageReader(p CoveragePeer, spool *os.Root) ClockCoverageReader {
	var mu sync.Mutex
	var next uint64
	broken := false
	unavailable := errors.New("clock-coverage-read-unavailable")
	return func(ctx context.Context, input operator.HostLeaseReadInput) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		deadline, bounded := ctx.Deadline()
		if broken || !bounded {
			return "", unavailable
		}
		args, err := json.Marshal(operator.HostLeaseReadArgs{Reads: []operator.HostLeaseReadInput{input}})
		if err != nil {
			return "", unavailable
		}
		next++
		id := strconv.FormatUint(next, 10)
		request := operator.Request{ID: id, Operation: operator.HostLeaseRead, Target: input.Target, Action: "clock", Args: args, DeadlineUnixMS: strconv.FormatInt(deadline.UnixMilli(), 10), RemainingMS: max(0, time.Until(deadline).Milliseconds())}
		if err := p.Send(ctx, operator.Frame{Type: "request", Session: CoverageSession, Request: &request}); err != nil {
			broken = true
			return "", unavailable
		}
		frame, err := p.Receive(ctx)
		if err != nil || ctx.Err() != nil || frame.Type != "result" || frame.Session != CoverageSession || frame.Result == nil || frame.Result.ID != id {
			// The peer may still deliver this reply; it cannot be told from the next one.
			broken = true
			return "", unavailable
		}
		// A correlated reply leaves the peer usable for the other hosts, even
		// when this host's read failed.
		reply, err := operatortransport.ResolveReply(spool, *frame.Result)
		if err != nil || reply.Transport != operator.Succeeded || reply.ExitCode == nil || *reply.ExitCode != 0 || reply.NotSubmitted {
			return "", unavailable
		}
		var output operator.HostOutput
		if observe.Decode(reply.Payload, &output, observe.Contract{"stdout"}) != nil || len(output.Stdout) > operator.MaxHostOutputBytes {
			return "", unavailable
		}
		return output.Stdout, nil
	}
}
