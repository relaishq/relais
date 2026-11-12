package operatorgates

import (
	"slices"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
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
// call count, the single call's owner slot and each worker slot's state.
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
		if i >= 0 {
			x := s.Workers[i]
			switch {
			case x.Recovering:
				code = "recovering"
			case x.Dead && x.Calls > 0:
				code = "dead-with-calls"
			case x.Dead:
				code = "dead"
			case x.Address != workers[w].Media:
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
// name itself in the gate's process evidence.
type gateWait struct {
	g       *procedure
	purpose string
	started time.Duration
	polls   int
	last    []string
}

func (g *procedure) startWait(purpose string) *gateWait {
	return &gateWait{g: g, purpose: purpose, started: g.p.Clock.Elapsed(), last: []string{"unobserved"}}
}

// poll passes the fixed purpose to the trace hook before each read.
func (w *gateWait) poll() {
	if w.g.p.Wait != nil {
		w.g.p.Wait(w.purpose)
	}
}

// observe counts one completed read and keeps its state codes.
func (w *gateWait) observe(state []string) {
	w.polls++
	w.last = state
}

// expired records this wait as the one the gate budget ended.
func (w *gateWait) expired() {
	w.g.evidence.ExpiredWait = &WaitDiagnostic{Purpose: w.purpose, WaitedMS: (w.g.p.Clock.Elapsed() - w.started).Milliseconds(), Polls: w.polls, Last: w.last}
}

// end returns err unchanged and records the wait when err is a budget end.
func (w *gateWait) end(err error) error {
	switch code(err) {
	case "gate-deadline", "precondition-failed:deadline-before-submission":
		w.expired()
	}
	return err
}
