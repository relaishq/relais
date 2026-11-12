package operatorgates

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

// WaitDiagnostic names a polling wait that the gate budget ended: the gate end
// passed, or the host-submission floor refused its next read. It is a
// diagnostic only and never changes a verdict, reason or category. Every
// field is a fixed public code or a count.
type WaitDiagnostic struct {
	Purpose  string `json:"purpose"`
	WaitedMS int64  `json:"waitedMs"`
	// Polls counts completed reads; Last holds the state codes of the final one.
	Polls int      `json:"polls"`
	Last  []string `json:"last"`
}

var waitPurposes = []string{
	"trial-takeover", "trial-settled", "trial-ready",
	"worker-settled", "worker-alive",
	"control-reconstructed", "control-reconstructed-degraded", "restart-takeover",
	"release-worker-settled", "release-worker-alive", "release-call-restored",
	"relay-rejoined", "relay-continuity",
	"worker-empty", "restore-worker-settled", "restore-worker-alive",
	"calls-settled", "failover-primary-recovered", "collection-complete", "boundary-idle",
}

var waitStates = []string{
	"unobserved",
	"control-ready", "control-not-ready",
	"calls-none", "calls-one", "calls-many",
	"call-owner-w1", "call-owner-w2", "call-owner-other",
	"w1-absent", "w1-recovering", "w1-dead-with-calls", "w1-dead", "w1-address-differs", "w1-liveness-unknown", "w1-draining", "w1-live",
	"w2-absent", "w2-recovering", "w2-dead-with-calls", "w2-dead", "w2-address-differs", "w2-liveness-unknown", "w2-draining", "w2-live",
	"relay-unmeasurable", "relay-same-instance", "relay-new-instance", "relay-epoch-same", "relay-epoch-advanced",
	"relay-workers-ready", "relay-workers-missing", "relay-routes-none", "relay-packets-none",
	"store-unobserved", "store-available", "store-unavailable", "store-primary-none", "store-primary-same", "store-primary-changed", "store-sync", "store-not-sync",
	"w1-local-unknown", "w1-local-busy", "w1-local-idle", "w2-local-unknown", "w2-local-busy", "w2-local-idle",
	"store-ownership-unknown", "store-ownership-busy", "store-ownership-idle",
}

func (w WaitDiagnostic) valid() bool {
	return slices.Contains(waitPurposes, w.Purpose) && w.WaitedMS >= 0 && w.Polls >= 0 && !slices.ContainsFunc(w.Last, func(s string) bool { return !slices.Contains(waitStates, s) })
}

// Public replaces any purpose or state outside the fixed codes.
func (w WaitDiagnostic) Public() WaitDiagnostic {
	if !slices.Contains(waitPurposes, w.Purpose) {
		w.Purpose = "unavailable"
	}
	w.WaitedMS, w.Polls = max(0, w.WaitedMS), max(0, w.Polls)
	last := make([]string, len(w.Last))
	for i, s := range w.Last {
		last[i] = "unavailable"
		if slices.Contains(waitStates, s) {
			last[i] = s
		}
	}
	w.Last = last
	return w
}

// controlState describes a control status by fixed codes only: readiness,
// call count, the single call's owner slot and each worker slot's state. A
// slot without a configured endpoint skips the address check.
func controlState(s controlplane.Status, workers map[string]WorkerEndpoint) []string {
	state := []string{"control-not-ready"}
	if s.Ready {
		state[0] = "control-ready"
	}
	switch len(s.Calls) {
	case 0:
		state = append(state, "calls-none")
	case 1:
		owner := "call-owner-other"
		if o := s.Calls[0].Owner; o == "w1" || o == "w2" {
			owner = "call-owner-" + o
		}
		state = append(state, "calls-one", owner)
	default:
		state = append(state, "calls-many")
	}
	for _, w := range []string{"w1", "w2"} {
		i := slices.IndexFunc(s.Workers, func(x controlplane.WorkerStatus) bool { return x.Name == w })
		code := "absent"
		endpoint, configured := workers[w]
		if i >= 0 {
			x := s.Workers[i]
			switch {
			case x.Recovering:
				code = "recovering"
			case x.Dead && x.Calls > 0:
				code = "dead-with-calls"
			case x.Dead:
				code = "dead"
			case configured && x.Address != endpoint.Media:
				code = "address-differs"
			case x.LivenessUnknown:
				code = "liveness-unknown"
			case x.Draining:
				code = "draining"
			default:
				code = "live"
			}
		}
		state = append(state, w+"-"+code)
	}
	return state
}

// observedState describes a measurement observation, which may lack a status.
func observedState(o *MeasurementObservation, workers map[string]WorkerEndpoint) []string {
	if o.Status == nil {
		return []string{"unobserved"}
	}
	return controlState(*o.Status, workers)
}

// storeState compares a store observation with the primary before failover.
func storeState(s StoreObservation, before string) []string {
	state := []string{"store-unavailable", "store-primary-none", "store-not-sync"}
	if s.Available {
		state[0] = "store-available"
	}
	switch s.Primary {
	case "":
	case before:
		state[1] = "store-primary-same"
	default:
		state[1] = "store-primary-changed"
	}
	if s.Durability == "sync" {
		state[2] = "store-sync"
	}
	return state
}

// localState describes a worker's local inventory for the boundary idle wait.
func localState(w string, l mediaworker.WorkerStatus) string {
	switch {
	case !l.InventoryKnown || l.Truncated || l.Inventory == nil:
		return w + "-local-unknown"
	case l.Sessions != 0 || len(l.Inventory) != 0:
		return w + "-local-busy"
	}
	return w + "-local-idle"
}

// relayState compares a relay inspection with the one taken before the restart.
func relayState(a, b relay.RelayStatus) []string {
	if !measurable(b) {
		return []string{"relay-unmeasurable"}
	}
	state := []string{"relay-same-instance", "relay-epoch-same", "relay-workers-missing"}
	if b.Instance != a.Instance {
		state[0] = "relay-new-instance"
	}
	if a.Continuity != nil && b.Continuity.Epoch > a.Continuity.Epoch {
		state[1] = "relay-epoch-advanced"
	}
	if b.Stats.Workers == 2 {
		state[2] = "relay-workers-ready"
	}
	if b.Stats.RoutesRestored == 0 {
		state = append(state, "relay-routes-none")
	}
	if b.Stats.WorkerPackets == 0 {
		state = append(state, "relay-packets-none")
	}
	return state
}

// gateWait follows one polling wait so that a wait the gate budget ends can
// name itself in the gate's evidence.
type gateWait struct {
	p       ProcedurePorts
	into    **WaitDiagnostic
	purpose string
	started time.Duration
	polls   int
	last    []string
}

// newGateWait records an expired wait into *into.
func newGateWait(p ProcedurePorts, into **WaitDiagnostic, purpose string) *gateWait {
	return &gateWait{p: p, into: into, purpose: purpose, started: p.Clock.Elapsed(), last: []string{"unobserved"}}
}

func (g *procedure) startWait(purpose string) *gateWait {
	return newGateWait(g.p, &g.evidence.ExpiredWait, purpose)
}

// poll passes the fixed purpose to the trace hook before each read.
func (w *gateWait) poll() {
	if w.p.Wait != nil {
		w.p.Wait(w.purpose)
	}
}

// observe counts one completed read and keeps its state codes.
func (w *gateWait) observe(state []string) {
	w.polls++
	w.last = state
}

// expired records this wait as the one the gate budget ended.
func (w *gateWait) expired() {
	*w.into = &WaitDiagnostic{Purpose: w.purpose, WaitedMS: (w.p.Clock.Elapsed() - w.started).Milliseconds(), Polls: w.polls, Last: w.last}
}

// end returns err unchanged and records the wait when err is a budget end.
func (w *gateWait) end(err error) error {
	if budgetEnded(err) {
		w.expired()
	}
	return err
}

// budgetEnded reports a gate-end refusal, a submission-floor refusal, or the
// gate context's deadline ending a read or sleep.
func budgetEnded(err error) bool {
	switch code(err) {
	case "gate-deadline", "precondition-failed:deadline-before-submission":
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
