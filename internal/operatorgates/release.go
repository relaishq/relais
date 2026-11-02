package operatorgates

import (
	"context"
	"slices"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/relay"
)

func RunReleaseGate(ctx context.Context, c *ProcessConfig, p ProcedurePorts, end time.Duration) ProcedureResult {
	g := newProcedure(c, p, end)
	g.end -= RestoreReserve
	return runReleaseGate(ctx, g)
}

func runReleaseGate(ctx context.Context, g *procedure) ProcedureResult {
	g.evidence.RelayWindows = "inconclusive"
	err := g.release(ctx)
	return g.finish(ctx, err, true)
}
func (g *procedure) releaseStatus(ctx context.Context, allowDown string) (controlplane.Status, error) {
	s, err := g.status(ctx)
	if err != nil {
		return s, err
	}
	reg := g.p.Broker.Registration()
	if !s.Ready || len(s.Calls) != 1 || reg == nil {
		return s, fault("missing-original-call")
	}
	for _, id := range reg.Sessions {
		if _, ok := findCall(s, id); !ok {
			return s, fault("missing-original-call")
		}
	}
	for _, w := range []string{"w1", "w2"} {
		if !slices.ContainsFunc(s.Workers, func(x controlplane.WorkerStatus) bool {
			return x.Name == w && x.Address == g.c.Parameters.Workers[w].Media && (w == allowDown || !x.Dead && !x.Recovering)
		}) {
			return s, fault("missing-live-workers")
		}
	}
	return s, nil
}
func (g *procedure) releaseWait(ctx context.Context, allowDown string, test func(controlplane.Status) bool) error {
	for g.p.Clock.Elapsed() < g.end {
		s, err := g.releaseStatus(ctx, allowDown)
		if err != nil {
			return err
		}
		if test(s) {
			return nil
		}
		if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fault("gate-deadline")
}
func (g *procedure) settle(ctx context.Context) error {
	if g.p.Clock.Elapsed()+CalibrationWarmup >= g.end {
		return fault("gate-deadline")
	}
	return g.p.Sleep(ctx, CalibrationWarmup)
}
func (g *procedure) releaseWorkers(ctx context.Context) (int, error) {
	moves := 0
	if _, err := g.do(ctx, "freeze", "shared", ProcessExtra{}); err != nil {
		return moves, err
	}
	for _, w := range []string{"w1", "w2"} {
		s, err := g.releaseStatus(ctx, "")
		if err != nil {
			return moves, err
		}
		call := s.Calls[0]
		var id *Identity
		if call.Owner == w {
			i := g.identity(call, "release-"+g.c.Target+"-"+w)
			id = &i
			if err = g.arm(ctx, *id); err != nil {
				return moves, err
			}
		}
		r, err := g.drain(ctx, w)
		if err != nil {
			return moves, err
		}
		if r.Drain.Moves == nil {
			return moves, fault("missing-drain-evidence")
		}
		if id != nil {
			if err = g.emitDrain(ctx, *id, r, true); err != nil {
				return moves, err
			}
			moves++
		}
		if err = WorkerEmpty(ctx, g.ports(), w, g.end); err != nil {
			return moves, err
		}
		if _, err = g.do(ctx, "worker-stop", w, ProcessExtra{Worker: w}); err != nil {
			return moves, err
		}
		if err = g.releaseWait(ctx, w, func(s controlplane.Status) bool { return settled(s, w) }); err != nil {
			return moves, err
		}
		if _, err = g.do(ctx, "replace", "shared", ProcessExtra{Worker: w}); err != nil {
			return moves, err
		}
		if _, err = g.do(ctx, "worker-start", w, ProcessExtra{Worker: w}); err != nil {
			return moves, err
		}
		if err = g.releaseWait(ctx, w, func(s controlplane.Status) bool { return alive(s, w, g.c.Parameters.Workers) }); err != nil {
			return moves, err
		}
		g.evidence.WorkerRollouts++
	}
	return moves, nil
}
func measurable(r relay.RelayStatus) bool {
	return r.Instance != "" && r.Public.IsValid() && r.Private.IsValid() && r.Continuity != nil
}
func (g *procedure) relay(ctx context.Context, strict bool, original int) error {
	before, err := g.do(ctx, "relay-inspect", "relay", ProcessExtra{})
	if err != nil {
		return err
	}
	a := before.Inspection.Status
	if strict {
		if a.Stats.SelfFences > 0 {
			return fault("known-relay-failure")
		}
		if !measurable(a) || a.Stats.WorkerPackets <= 0 {
			return fault("missing-relay-evidence")
		}
	}
	if _, err = g.do(ctx, "relay-stage", "relay", ProcessExtra{}); err != nil {
		return err
	}
	if _, err = g.do(ctx, "relay-stop", "relay", ProcessExtra{ActiveUnit: before.Inspection.Active}); err != nil {
		return err
	}
	var after StepResult
	for g.p.Clock.Elapsed() < g.end {
		r, e := g.do(ctx, "relay-inspect", "relay", ProcessExtra{})
		// An unknown result latches even inside an observation loop. Only a
		// definite pure-read failure may be observed again.
		if e != nil {
			reason, category := readFailure(e)
			if g.unknown || reason == "" || category == "observation" {
				return e
			}
		} else {
			after = r
			b := r.Inspection.Status
			if strict {
				if b.Stats.SelfFences > 0 {
					return fault("known-relay-failure")
				}
				if !measurable(b) {
					return fault("missing-relay-evidence")
				}
				if b.Instance != a.Instance && b.Continuity.Epoch > a.Continuity.Epoch && b.Stats.Workers == 2 && b.Stats.RoutesRestored >= 1 && b.Stats.WorkerPackets > 0 {
					break
				}
			} else if b.Stats.Workers == 2 {
				break
			}
		}
		if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	b := after.Inspection.Status
	if b.Stats.SelfFences > 0 {
		return fault("relay-self-fence")
	}
	if strict {
		if !measurable(b) || b.Public != a.Public || b.Private != a.Private {
			return fault("missing-relay-continuity")
		}
	} else {
		if a.Instance != "" && b.Instance != "" && (b.Instance == a.Instance || b.Public != a.Public || b.Private != a.Private || a.Continuity != nil && b.Continuity != nil && b.Continuity.Epoch <= a.Continuity.Epoch || b.Stats.RoutesRestored == 0 || b.Stats.WorkerPackets == 0) {
			return fault("relay-continuity-failed")
		}
		if !measurable(a) || !measurable(b) || b.Stats.Workers != 2 || b.Stats.RoutesRestored < uint64(original) || a.Stats.WorkerPackets <= 0 {
			return fault("missing-relay-evidence")
		}
		delta := b.Continuity.Epoch - a.Continuity.Epoch
		routes := b.Stats.RoutesRestored
		g.evidence.RelayEpochDelta = &delta
		g.evidence.RoutesRestored = &routes
	}
	if _, err = g.do(ctx, "relay-replenish", "relay", ProcessExtra{}); err != nil {
		return err
	}
	g.evidence.Restarts++
	return nil
}
func (g *procedure) releaseControl(ctx context.Context) error {
	before, err := g.releaseStatus(ctx, "")
	if err != nil {
		return err
	}
	if _, err = g.do(ctx, "control-stop", "shared", ProcessExtra{}); err != nil {
		return err
	}
	if _, err = g.do(ctx, "control-start", "shared", ProcessExtra{}); err != nil {
		return err
	}
	err = g.releaseWait(ctx, "", func(s controlplane.Status) bool {
		return slices.ContainsFunc(s.Calls, func(c controlplane.CallStatus) bool {
			return c.ID == before.Calls[0].ID && c.Owner == before.Calls[0].Owner && c.Epoch == before.Calls[0].Epoch
		})
	})
	if err == nil {
		g.evidence.Restarts++
	}
	return err
}
func (g *procedure) signal(ctx context.Context) error {
	if _, err := g.do(ctx, "signal-stop", "shared", ProcessExtra{}); err != nil {
		return err
	}
	_, err := g.do(ctx, "signal-start", "shared", ProcessExtra{})
	return err
}
func (g *procedure) release(ctx context.Context) error {
	reg := g.p.Broker.Registration()
	if reg == nil || !reg.Near || g.p.Clock.Elapsed() >= g.end {
		return fault("gate-deadline")
	}
	if err := TrialWarmup(ctx, g.p, g.end); err != nil {
		return err
	}
	if _, err := g.releaseStatus(ctx, ""); err != nil {
		return err
	}
	g.c.Mixed = true
	for _, target := range []string{"to", "from"} {
		g.c.Target = target
		g.c.Operation = "upgrade"
		if target == "from" {
			g.c.Operation = "rollback"
		}
		moves := 0
		var err error
		if target == "to" {
			moves, err = g.releaseWorkers(ctx)
			if err == nil {
				err = g.relay(ctx, true, 1)
			}
			if err == nil {
				err = g.releaseControl(ctx)
			}
			if err == nil {
				err = g.signal(ctx)
			}
		} else {
			err = g.signal(ctx)
			if err == nil {
				err = g.releaseControl(ctx)
			}
			if err == nil {
				err = g.relay(ctx, true, 1)
			}
			if err == nil {
				err = g.settle(ctx)
			}
			if err == nil {
				moves, err = g.releaseWorkers(ctx)
			}
			if err == nil {
				_, err = g.do(ctx, "unfreeze", "shared", ProcessExtra{})
			}
		}
		if err != nil {
			return err
		}
		if g.p.Inventory == nil {
			return fault("missing-target-inventory")
		}
		if err = g.p.Inventory(ctx, target, g.end); err != nil {
			return err
		}
		n := 1
		if g.evidence.ReleaseTargetsVerified != nil {
			n += *g.evidence.ReleaseTargetsVerified
		}
		g.evidence.ReleaseTargetsVerified = &n
		if err = g.settle(ctx); err != nil {
			return err
		}
		takeovers := 2
		if target == "from" {
			takeovers = 1
		}
		if err = g.trials(ctx, TrialTarget{max(0, 3-moves), takeovers}, "stable-"+target); err != nil {
			return err
		}
	}
	return nil
}
