package operatorgates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type TrialTarget struct {
	Moves     int `json:"moves"`
	Takeovers int `json:"takeovers"`
}

func TrialTargets(g operator.Gate) TrialTarget {
	switch g {
	case operator.Rollout:
		return TrialTarget{7, 3}
	case operator.ControlRestart:
		return TrialTarget{7, 4}
	case operator.UpgradeRollback:
		return TrialTarget{6, 3}
	}
	return TrialTarget{}
}
func TrialWarmup(ctx context.Context, p ProcedurePorts, end time.Duration) error {
	r := p.Broker.Registration()
	if r == nil || r.RegisteredAt == nil || *r.RegisteredAt > maxSafeEpoch || *r.RegisteredAt < -maxSafeEpoch {
		return fault("Missing pre-fault registration")
	}
	ready, media := *r.RegisteredAt, *r.RegisteredAt
	if r.ReadyObservedAt != nil {
		ready = *r.ReadyObservedAt
	}
	if r.MediaStartedAt != nil {
		media = *r.MediaStartedAt
	}
	until := time.UnixMilli(max(ready+FaultCallAge.Milliseconds(), media+CalibrationWarmup.Milliseconds()))
	wait := until.Sub(p.Clock.Wall())
	if wait >= end-p.Clock.Elapsed() {
		return fault("gate-deadline")
	}
	if wait > 0 {
		return p.Sleep(ctx, wait)
	}
	return nil
}

// Canonicalize object order only. Preserve omission, explicit false, timestamps
// and unknown product members: all belong to the pre-fault history identity.
func historyKey(raw json.RawMessage) string {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func freshTakeovers(before, after StepResult, id, from string) []int {
	old := map[string]bool{}
	for _, r := range before.History {
		old[historyKey(r)] = true
	}
	var candidates []int
	for i, m := range after.Status.Takeovers {
		if m.ID == id && m.From == from && i < len(after.History) && !old[historyKey(after.History[i])] {
			candidates = append(candidates, i)
		}
	}
	return candidates
}
func WorkerTrials(ctx context.Context, c *ProcessConfig, p ProcedurePorts, end time.Duration, target TrialTarget, prefix string) error {
	return newProcedure(c, p, end).trials(ctx, target, prefix)
}
func (g *procedure) trials(ctx context.Context, target TrialTarget, prefix string) error {
	status := func() (StepResult, error) {
		r, err := g.do(ctx, "status", "shared", ProcessExtra{})
		if err == nil && (!r.LossKnown || !r.Status.Ready || r.Status.Pool.Cap != 2) {
			err = fault("missing-trial-inventory")
		}
		return r, err
	}
	wait := func(test func(StepResult) bool) (StepResult, error) {
		for g.p.Clock.Elapsed() < g.end {
			r, err := status()
			if err != nil {
				return r, err
			}
			if test(r) {
				return r, nil
			}
			if err = g.p.Sleep(ctx, 500*time.Millisecond); err != nil {
				return r, err
			}
		}
		return StepResult{}, fault("gate-deadline")
	}
	for _, batch := range []struct {
		kind  string
		count int
	}{{"move", target.Moves}, {"takeover", target.Takeovers}} {
		for i := 0; i < batch.count; i++ {
			before, err := status()
			if err != nil {
				return err
			}
			reg := g.p.Broker.Registration()
			if reg == nil || !reg.Near || len(reg.Sessions) != 1 || len(before.Status.Calls) != 1 || !alive(before.Status, "w1", g.c.Parameters.Workers) || !alive(before.Status, "w2", g.c.Parameters.Workers) {
				return fault("missing-trial-call")
			}
			call := before.Status.Calls[0]
			if !validCall(call) || call.ID != reg.Sessions[0] {
				return fault("missing-trial-identity")
			}
			to := other(call.Owner)
			id := g.identity(call, fmt.Sprintf("%s-%s-%d", prefix, batch.kind, i))
			if err = g.arm(ctx, id); err != nil {
				return err
			}
			var receipt Receipt
			if batch.kind == "move" {
				moved, err := g.do(ctx, "move", "shared", ProcessExtra{Worker: to, Session: call.ID, Owner: call.Owner, Epoch: call.Epoch})
				if err != nil {
					return err
				}
				r := moved.Move
				if r.Lost {
					return fault("known-call-loss")
				}
				if r.ID != call.ID || r.From != call.Owner || r.To != to || r.Kind != "move" || r.Error != "" || r.Start.IsZero() || r.End.IsZero() {
					return fault("missing-move-receipt")
				}
				receipt = Receipt{id, "move", r.Start, r.End, moved.Raw, false}
			} else {
				killed, err := g.do(ctx, "worker-down", call.Owner, ProcessExtra{Worker: call.Owner})
				if err != nil {
					return err
				}
				recovered, err := wait(func(r StepResult) bool {
					c, ok := findCall(r.Status, call.ID)
					return ok && c.Owner == to && c.Epoch > call.Epoch && len(freshTakeovers(before, r, call.ID, call.Owner)) > 0
				})
				if err != nil {
					return err
				}
				candidates := freshTakeovers(before, recovered, call.ID, call.Owner)
				for _, i := range candidates {
					if recovered.Status.Takeovers[i].Lost {
						return fault("known-call-loss")
					}
				}
				if len(candidates) != 1 {
					return fault("missing-takeover-receipt")
				}
				i := candidates[0]
				r := recovered.Status.Takeovers[i]
				if r.Kind != "takeover" || r.To != to || r.Error != "" || r.End.IsZero() {
					return fault("missing-takeover-receipt")
				}
				receipt = Receipt{id, "takeover", killed.Started, r.End, recovered.History[i], false}
			}
			if err = g.feed(ctx, receipt); err != nil {
				return err
			}
			if err = g.consent(ctx); err != nil {
				return err
			}
			if batch.kind == "takeover" {
				if _, err = wait(func(r StepResult) bool { return settled(r.Status, call.Owner) }); err != nil {
					return err
				}
				if _, err = g.do(ctx, "replace", "shared", ProcessExtra{Worker: call.Owner}); err != nil {
					return err
				}
				if _, err = g.do(ctx, "worker-start", call.Owner, ProcessExtra{Worker: call.Owner}); err != nil {
					return err
				}
			}
			if _, err = wait(func(r StepResult) bool {
				c, ok := findCall(r.Status, call.ID)
				return alive(r.Status, "w1", g.c.Parameters.Workers) && alive(r.Status, "w2", g.c.Parameters.Workers) && ok && c.Owner == to && c.Epoch > call.Epoch
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
