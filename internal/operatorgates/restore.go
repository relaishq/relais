package operatorgates

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
)

// WorkerEmpty uses one absolute retry window. Control status is effectful;
// only a named definite precondition failure can be observed again.
func WorkerEmpty(ctx context.Context, p ProcedurePorts, w string, end time.Duration) error {
	until := min(end-30*time.Second, p.Clock.Elapsed()+WorkerEmptyWindow)
	last := fault("gate-deadline")
	fits := func() bool { return until-p.Clock.Elapsed() >= WorkerEmptyReserve }
	for fits() {
		r, err := p.Step(ctx, "status", "shared", until, ProcessExtra{})
		if err == nil {
			s := r.Status
			if s.LostCount > 0 {
				return fault("known-call-loss")
			}
			reason := ""
			switch {
			case !s.Ready || s.Pool.Reservations != 0:
				reason = "control-not-settled"
			case !observe.Draining(s, w):
				reason = "not-draining"
			case slices.ContainsFunc(s.Calls, func(c controlplane.CallStatus) bool { return c.Owner == w }):
				reason = "control-owner"
			}
			if reason != "" {
				err = fault("precondition-failed:" + reason)
			} else if !fits() {
				break
			} else {
				_, err = p.Step(ctx, "worker-empty", "shared", until, ProcessExtra{Worker: w})
				if err == nil {
					return nil
				}
			}
		}
		if !strings.HasPrefix(code(err), "precondition-failed:") {
			return err
		}
		last = err
		if err = p.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return last
}

// restoreWorkers never runs after uncertainty. A settled death uses the same
// replace/start path; an alive worker needs no mutation.
func (g *procedure) restoreWorkers(ctx context.Context) error {
	if g.unknown {
		return fault("command-result-unknown")
	}
	for _, w := range slices.Clone(g.down) {
		until := min(g.end-30*time.Second, g.p.Clock.Elapsed()+90*time.Second)
		s, err := g.status(ctx)
		if err != nil {
			return err
		}
		for !alive(s, w, g.c.Parameters.Workers) && !settled(s, w) && g.p.Clock.Elapsed() < until {
			if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			s, err = g.status(ctx)
			if err != nil {
				return err
			}
		}
		if alive(s, w, g.c.Parameters.Workers) {
			g.down = slices.DeleteFunc(g.down, func(x string) bool { return x == w })
			continue
		}
		if !settled(s, w) {
			return fault("restore-unsettled")
		}
		if _, err = g.do(ctx, "replace", "shared", ProcessExtra{Worker: w}); err != nil {
			return err
		}
		if _, err = g.do(ctx, "worker-start", w, ProcessExtra{Worker: w}); err != nil {
			return err
		}
		until = min(g.end-30*time.Second, g.p.Clock.Elapsed()+120*time.Second)
		for !alive(s, w, g.c.Parameters.Workers) {
			if g.p.Clock.Elapsed() >= until {
				return fault("restore-unconfirmed")
			}
			if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			s, err = g.status(ctx)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func (g *procedure) restoreGate(ctx context.Context) error {
	if g.unknown {
		return fault("command-result-unknown")
	}
	var failure error
	if len(g.down) > 0 {
		failure = g.restoreWorkers(ctx)
		if code(failure) == "command-result-unknown" {
			return failure
		}
	}
	if g.frozen {
		if _, err := g.do(ctx, "unfreeze", "shared", ProcessExtra{}); err != nil {
			return err
		}
	}
	return failure
}
