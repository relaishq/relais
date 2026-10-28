package operatorgates

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

func RunProcessGate(ctx context.Context, gate operator.Gate, c *ProcessConfig, p ProcedurePorts, end time.Duration) ProcedureResult {
	g := newProcedure(c, p, end)
	err := g.process(ctx, gate)
	result := g.finish(ctx, err, false)
	if gate == operator.RelayDeploy {
		if p.Duplicates != nil {
			g.evidence.DuplicatePackets = p.Duplicates()
		}
		g.evidence.RelayLocalTiming = "inconclusive"
		g.evidence.RelayConnectionInvariants = "inconclusive"
		if result.Status != operator.Failure {
			result.Status = operator.Inconclusive
			result.AwaitingPool = false
		}
		if g.evidence.DuplicatePackets != nil && *g.evidence.DuplicatePackets > 0 {
			result.Status = operator.Failure
		}
	}
	if (result.Status == operator.Passed || result.AwaitingPool) && gate == operator.ControlRestart {
		s, err := g.status(ctx)
		reg := p.Broker.Registration()
		if err != nil || slices.ContainsFunc(s.Calls, func(c controlplane.CallStatus) bool { return slices.Contains(reg.Sessions, c.ID) }) || g.evidence.ControlReadyMS == nil {
			result.Status = operator.Inconclusive
			if code(err) == "known-call-loss" || s.LostCount > 0 {
				result.Status = operator.Failure
			}
			result.AwaitingPool = false
			result.Completed = false
		}
		if g.unknown {
			result.Reason = "command-result-unknown"
		}
	}
	result.Process = g.evidence
	return result
}
func (g *procedure) finish(ctx context.Context, err error, release bool) ProcedureResult {
	r := ProcedureResult{Status: operator.Inconclusive}
	if err == nil {
		r.Status = operator.Passed
	}
	switch code(err) {
	case "known-call-loss", "compatibility-refused", "known-relay-failure", "relay-self-fence", "relay-continuity-failed":
		r.Status = operator.Failure
	case "command-result-unknown":
		r.Reason = code(err)
	case "gate-deadline":
		if release {
			r.Reason = code(err)
		}
	}
	if reason, category := readFailure(err); reason != "" {
		r.Reason = reason
		r.Category = category
	}
	// Definite errors and the normal completed procedure can restore owned state.
	// An unknown effect cannot authorize even the first restoration request.
	restored := true
	if !g.unknown && r.Reason != "command-result-unknown" {
		if restore := g.restoreGate(ctx); restore != nil {
			restored = false
			switch code(restore) {
			case "command-result-unknown":
				r.Reason = code(restore)
			case "known-call-loss":
				r.Status = operator.Failure
			}
		}
	}
	procedurePassed := r.Status == operator.Passed
	if g.p.Media != nil {
		media, err := g.p.Media(ctx)
		if code(err) == "command-result-unknown" {
			g.unknown = true
			procedurePassed = false
		}
		r.Media = media
		if media != nil && media.Status == operator.Failure {
			r.Status = operator.Failure
		} else if err != nil || media == nil || media.Status != operator.Passed {
			if r.Status != operator.Failure {
				r.Status = operator.Inconclusive
			}
		}
	}
	if r.Media == nil && r.Status != operator.Failure {
		r.Status = operator.Inconclusive
	}
	if g.c.Pool && procedurePassed && r.Media != nil && r.Status != operator.Failure {
		r.Status = operator.Inconclusive
		r.AwaitingPool = true
	}
	if release && err == nil && r.Media != nil && r.Status != operator.Failure {
		r.Status = operator.Inconclusive
		r.AwaitingPool = true
	}
	if g.p.Clock.Elapsed() >= g.end && r.Status != operator.Failure {
		r.Status = operator.Inconclusive
		r.Reason = "gate-deadline"
		r.AwaitingPool = false
	}
	if g.unknown {
		r.Reason = "command-result-unknown"
		r.AwaitingPool = false
	}
	r.Completed = procedurePassed && restored && !g.unknown && r.Reason == "" && r.Category == ""
	r.Process = g.evidence
	return r
}
func (g *procedure) process(ctx context.Context, gate operator.Gate) error {
	reg := g.p.Broker.Registration()
	if g.p.Clock.Elapsed() >= g.end || reg == nil || !reg.Near || len(reg.Sessions) == 0 {
		return fault("gate-deadline")
	}
	if g.c.Trials {
		if err := TrialWarmup(ctx, g.p, g.end); err != nil {
			return err
		}
	}
	before, err := g.status(ctx)
	if err != nil {
		return err
	}
	if !before.Ready || before.Pool.Cap != 2 || len(before.Calls) != 1 || !alive(before, "w1", g.c.Parameters.Workers) || !alive(before, "w2", g.c.Parameters.Workers) {
		return fault("missing-live-call")
	}
	for _, id := range reg.Sessions {
		c, ok := findCall(before, id)
		if !ok || !validCall(c) {
			return fault("missing-live-call")
		}
	}
	switch gate {
	case operator.Rollout:
		if _, err = g.do(ctx, "freeze", "shared", ProcessExtra{}); err != nil {
			return err
		}
		for _, w := range []string{"w1", "w2"} {
			s, err := g.status(ctx)
			if err != nil {
				return err
			}
			var ids []Identity
			for _, c := range s.Calls {
				if c.Owner == w {
					ids = append(ids, g.identity(c, "drain-"+w))
				}
			}
			if len(ids) > 0 {
				if err = g.armIdentities(ctx, "drain-"+w, ids); err != nil {
					return err
				}
			}
			drained, err := g.drain(ctx, w)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err = g.emitDrain(ctx, id, drained, false); err != nil {
					return err
				}
			}
			if err = WorkerEmpty(ctx, g.ports(), w, g.end); err != nil {
				return err
			}
			if _, err = g.do(ctx, "worker-stop", w, ProcessExtra{Worker: w}); err != nil {
				return err
			}
			if err = g.replace(ctx, w); err != nil {
				return err
			}
			g.evidence.WorkerRollouts++
		}
		if _, err = g.do(ctx, "unfreeze", "shared", ProcessExtra{}); err != nil {
			return err
		}
	case operator.RelayDeploy:
		if err = g.relay(ctx, false, len(reg.Sessions)); err != nil {
			return err
		}
	case operator.ControlRestart:
		if err = g.controlRestart(ctx, before); err != nil {
			return err
		}
	default:
		return fault("invalid-gate")
	}
	if g.c.Trials && gate != operator.RelayDeploy {
		target := TrialTargets(gate)
		for _, r := range g.p.Broker.Events() {
			switch r.Kind {
			case "move":
				target.Moves--
			case "takeover":
				target.Takeovers--
			}
		}
		target.Moves = max(0, target.Moves)
		target.Takeovers = max(0, target.Takeovers)
		return g.trials(ctx, target, string(gate))
	}
	return nil
}
func (g *procedure) ports() ProcedurePorts { p := g.p; p.Step = g.step; return p }
func (g *procedure) controlRestart(ctx context.Context, before controlplane.Status) error {
	call := before.Calls[0]
	owner, to := call.Owner, other(call.Owner)
	reconstructed := func(s controlplane.Status) bool {
		c, ok := findCall(s, call.ID)
		return s.Ready && ok && c.Owner == owner && c.Epoch == call.Epoch && slices.ContainsFunc(s.Workers, func(w controlplane.WorkerStatus) bool {
			return w.Name == "w1" && w.Address == g.c.Parameters.Workers["w1"].Media
		}) && slices.ContainsFunc(s.Workers, func(w controlplane.WorkerStatus) bool {
			return w.Name == "w2" && w.Address == g.c.Parameters.Workers["w2"].Media
		})
	}
	if _, err := g.do(ctx, "control-stop", "shared", ProcessExtra{}); err != nil {
		return err
	}
	normal, err := g.do(ctx, "control-start", "shared", ProcessExtra{})
	if err != nil {
		return err
	}
	g.evidence.ControlReadyMS = normal.ReadyMS
	if _, err = g.wait(ctx, reconstructed); err != nil {
		return err
	}
	g.evidence.Restarts++
	if _, err = g.do(ctx, "worker-down", to, ProcessExtra{Worker: to}); err != nil {
		return err
	}
	if _, err = g.do(ctx, "control-stop", "shared", ProcessExtra{}); err != nil {
		return err
	}
	started, err := g.do(ctx, "control-start", "shared", ProcessExtra{})
	if err != nil {
		return err
	}
	if normal.ReadyMS == nil || started.ReadyMS == nil || *normal.ReadyMS < 0 || *started.ReadyMS < 0 {
		return fault("missing-readiness-evidence")
	}
	ready := max(*normal.ReadyMS, *started.ReadyMS)
	met := 0
	if ready <= 5000 {
		met = 1
	}
	g.evidence.ControlReadyMS = &ready
	g.evidence.ControlReadyNormalMS = normal.ReadyMS
	g.evidence.ControlReadyDegradedMS = started.ReadyMS
	g.evidence.ControlReadyTargetMet = &met
	if _, err = g.wait(ctx, reconstructed); err != nil {
		return err
	}
	// Restore the empty second worker before any move of the original call.
	if err = g.replace(ctx, to); err != nil {
		return err
	}
	current, err := g.status(ctx)
	if err != nil {
		return err
	}
	c, ok := findCall(current, call.ID)
	if !ok {
		return fault("missing-original-call")
	}
	id := g.identity(c, "restart-move")
	id.Worker = owner
	if err = g.arm(ctx, id); err != nil {
		return err
	}
	moved, err := g.drain(ctx, owner)
	if err != nil {
		return err
	}
	if err = g.emitDrain(ctx, id, moved, false); err != nil {
		return err
	}
	if err = WorkerEmpty(ctx, g.ports(), owner, g.end); err != nil {
		return err
	}
	if _, err = g.do(ctx, "worker-stop", owner, ProcessExtra{Worker: owner}); err != nil {
		return err
	}
	if err = g.replace(ctx, owner); err != nil {
		return err
	}
	after, err := g.do(ctx, "status", "shared", ProcessExtra{})
	if err != nil {
		return err
	}
	c, ok = findCall(after.Status, call.ID)
	if !ok || c.Owner != to || c.Epoch <= id.Epoch || c.Epoch > maxSafeEpoch {
		return fault("missing-move-evidence")
	}
	takeover := g.identity(c, "restart-takeover")
	if err = g.arm(ctx, takeover); err != nil {
		return err
	}
	killed, err := g.do(ctx, "worker-down", to, ProcessExtra{Worker: to})
	if err != nil {
		return err
	}
	// New ownership and a fresh receipt must describe this fault, not a prior kill.
	recovered, err := g.waitStep(ctx, func(r StepResult) bool {
		c, ok := findCall(r.Status, call.ID)
		return ok && c.Owner == owner && c.Epoch > takeover.Epoch && len(freshTakeovers(after, r, call.ID, to)) > 0
	})
	if err != nil {
		return err
	}
	candidates := freshTakeovers(after, recovered, call.ID, to)
	if len(candidates) != 1 {
		return fault("missing-takeover-receipt")
	}
	i := candidates[0]
	m := recovered.Status.Takeovers[i]
	if m.Lost {
		return fault("known-call-loss")
	}
	if m.Kind != "takeover" || m.To != owner || m.Error != "" || m.End.IsZero() {
		return fault("missing-takeover-receipt")
	}
	// The signal bracket supplies the start even when control omitted it.
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(recovered.History[i], &raw); err != nil {
		return err
	}
	raw["start"], _ = json.Marshal(killed.Started)
	receiptRaw, _ := json.Marshal(raw)
	if err = g.feed(ctx, Receipt{takeover, "takeover", killed.Started, m.End, receiptRaw, false}); err != nil {
		return err
	}
	if g.c.Trials {
		if err = g.consent(ctx); err != nil {
			return err
		}
	}
	if err = g.replace(ctx, to); err != nil {
		return err
	}
	g.evidence.Restarts++
	return nil
}
