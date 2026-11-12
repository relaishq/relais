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

// workerEmpty runs WorkerEmpty as a named gate wait. WorkerEmpty returns a
// precondition code or gate-deadline only after its window or the gate budget
// ran out, so either records the expired wait; the error is unchanged.
func (g *procedure) workerEmpty(ctx context.Context, w string) error {
	wait := g.startWait("worker-empty")
	p := g.ports()
	p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
		if op != "status" {
			return g.step(ctx, op, slot, end, extra)
		}
		wait.poll()
		r, err := g.step(ctx, op, slot, end, extra)
		if err == nil {
			wait.observe(controlState(r.Status, g.c.Parameters.Workers))
		}
		return r, err
	}
	err := WorkerEmpty(ctx, p, w, g.end)
	if strings.HasPrefix(code(err), "precondition-failed:") {
		wait.expired()
	}
	return wait.end(err)
}

// restoreWorkers never runs after uncertainty. A settled death uses the same
// replace/start path; an alive worker needs no mutation.
func (g *procedure) restoreWorkers(ctx context.Context) error {
	if g.unknown {
		return fault("command-result-unknown")
	}
	// A restoration wait that runs out its window or the gate budget is
	// recorded as the expired wait; its own error code is unchanged.
	read := func(wait *gateWait) (controlplane.Status, error) {
		wait.poll()
		s, err := g.status(ctx)
		if err == nil {
			wait.observe(controlState(s, g.c.Parameters.Workers))
		}
		return s, wait.end(err)
	}
	for _, w := range slices.Clone(g.down) {
		// A confirmed stop has no running predecessor even while control still
		// reports its last live heartbeat. Wait for death before replace/start.
		stopped := slices.Contains(g.stopped, w)
		until := min(g.end-30*time.Second, g.p.Clock.Elapsed()+90*time.Second)
		settle := g.startWait("restore-worker-settled")
		s, err := read(settle)
		if err != nil {
			return err
		}
		for (stopped || !alive(s, w, g.c.Parameters.Workers)) && !settled(s, w) && g.p.Clock.Elapsed() < until {
			if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
				return settle.end(err)
			}
			s, err = read(settle)
			if err != nil {
				return err
			}
		}
		if !stopped && alive(s, w, g.c.Parameters.Workers) {
			g.down = slices.DeleteFunc(g.down, func(x string) bool { return x == w })
			continue
		}
		if !settled(s, w) {
			settle.expired()
			return fault("restore-unsettled")
		}
		if _, err = g.do(ctx, "replace", "shared", ProcessExtra{Worker: w}); err != nil {
			return err
		}
		if _, err = g.do(ctx, "worker-start", w, ProcessExtra{Worker: w}); err != nil {
			return err
		}
		until = min(g.end-30*time.Second, g.p.Clock.Elapsed()+120*time.Second)
		ready := g.startWait("restore-worker-alive")
		for !alive(s, w, g.c.Parameters.Workers) {
			if g.p.Clock.Elapsed() >= until {
				ready.expired()
				return fault("restore-unconfirmed")
			}
			if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
				return ready.end(err)
			}
			s, err = read(ready)
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
	// Return only the reserved part of the original window. Restore signaling
	// before worker waits, which can consume the rest of this bounded window.
	g.end = g.restoreEnd
	var failure error
	if g.frozen {
		_, failure = g.do(ctx, "unfreeze", "shared", ProcessExtra{})
		if code(failure) == "command-result-unknown" {
			return failure
		}
	}
	if len(g.down) > 0 {
		err := g.restoreWorkers(ctx)
		if code(err) == "known-call-loss" {
			g.restoreLoss = true
		}
		if failure == nil {
			failure = err
		}
	}
	return failure
}
