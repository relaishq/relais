package operatorgates

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
)

// GatePrecondition runs before a caller exists. Repairs use only existing
// control-start and empty/stop/replace/start procedures. Unknown control reads
// cannot be treated as absence: /status can reconcile ownership.
func GatePrecondition(ctx context.Context, c *ProcessConfig, p ProcedurePorts, end time.Duration) error {
	g := newProcedure(c, p, end)
	read := func() (controlplane.Status, bool, error) {
		inventoryOK := false
		if p.Inventory != nil {
			err := p.Inventory(ctx, "from", g.end)
			if code(err) == "command-result-unknown" {
				return controlplane.Status{}, false, err
			}
			inventoryOK = err == nil
		}
		s, err := g.status(ctx)
		if err != nil {
			return s, false, err
		}
		healthy := inventoryOK && s.Ready && s.LostCount == 0
		for _, w := range []string{"w1", "w2"} {
			healthy = healthy && alive(s, w, c.Parameters.Workers) && !observe.Draining(s, w)
		}
		return s, healthy, nil
	}
	s, healthy, err := read()
	if code(err) == "command-result-unknown" {
		return err
	}
	if healthy {
		return nil
	}
	if err != nil {
		if _, err = g.do(ctx, "control-start", "shared", ProcessExtra{}); err != nil {
			return err
		}
		s, healthy, err = read()
		if code(err) == "command-result-unknown" {
			return err
		}
	}
	for _, w := range []string{"w1", "w2"} {
		if !alive(s, w, c.Parameters.Workers) {
			g.down = append(g.down, w)
			continue
		}
		if observe.Draining(s, w) {
			if err = WorkerEmpty(ctx, g.ports(), w, g.end); err != nil {
				if _, category := readFailure(err); category != "" {
					return fault("precondition-unhealthy")
				}
				return err
			}
			if _, err = g.do(ctx, "worker-stop", w, ProcessExtra{Worker: w}); err != nil {
				return err
			}
		}
	}
	if len(g.down) > 0 {
		if err = g.restoreWorkers(ctx); code(err) == "command-result-unknown" {
			return err
		}
		_, healthy, err = read()
		if code(err) == "command-result-unknown" {
			return err
		}
	}
	if !healthy {
		return fault("precondition-unhealthy")
	}
	return nil
}
