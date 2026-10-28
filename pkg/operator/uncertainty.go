package operator

import "regexp"

// Uncertainty identifies a failed certainty boundary without retaining payloads,
// mailbox keys, scripts, addresses, credentials or free-form errors.
type Uncertainty struct {
	Kind           string `json:"kind"`
	RequestID      string `json:"requestId"`
	Target         string `json:"target"`
	CallerID       string `json:"callerId,omitempty"`
	Action         string `json:"action,omitempty"`
	DeadlineUnixMS string `json:"deadlineUnixMs"`
	RemainingMS    int64  `json:"remainingMs"`
	Rule           string `json:"rule"`
}

type UncertaintyRecorder interface{ RecordUncertainty(Uncertainty) }

// Public is also applied when reading a report, so adapter text cannot enter
// these fields through a manually constructed diagnostic.
func (u Uncertainty) Public() Uncertainty {
	switch u.Kind {
	case "host.execute", "status.read", "caller.ready", "caller.start", "caller.stop", "caller.feed", "mailbox.read", "mailbox.create", "caller.collect", "judge", "store.observe", "store.failover", "session", "gate", "gate.prepare":
	default:
		u.Kind = "unavailable"
	}
	decimal := regexp.MustCompile(`^[0-9]{1,20}$`)
	for _, p := range []*string{&u.RequestID, &u.CallerID, &u.DeadlineUnixMS} {
		if *p != "" && !decimal.MatchString(*p) {
			*p = "unavailable"
		}
	}
	if u.RequestID == "" {
		u.RequestID = "unavailable"
	}
	if u.DeadlineUnixMS == "" {
		u.DeadlineUnixMS = "unavailable"
	}
	switch u.Target {
	case "relay", "w1", "w2", "shared", "caller", "store", "judge", "session":
	default:
		u.Target = "unavailable"
	}
	switch u.Action {
	case "", "ready", "start", "stop", "collect", "latch", "judge", "observe", "failover", "caller-token.json", "transport.json", "setup-ready.json", "begin.json", "ready.json", "registration.json", "caller-probes", "caller-start", "ack", "hangup", "inventory", "clock", "telemetry", "signal-stop", "signal-start", "move", "status", "store-status", "worker-status", "relay-status", "relay-inspect", "worker-empty", "freeze", "unfreeze", "drain", "worker-stop", "replace", "worker-start", "control-stop", "control-start", "worker-down", "relay-stage", "relay-stop", "relay-replenish":
	default:
		if !Gate(u.Action).Valid() {
			u.Action = "operation"
		}
	}
	switch u.Rule {
	case "timeout", "cancelled", "lost-reply", "send-failed", "malformed-reply", "invalid-blob", "truncated-output", "missing-exit-status", "unconfirmed-exit", "operation-evidence-unknown", "transport-unknown", "read-failed", "create-unknown", "create-conflict", "unconfirmed-stop", "caller-stopped", "incomplete-evidence", "judge-unavailable", "unattributed-latch":
	default:
		u.Rule = "operation-evidence-unknown"
	}
	return u
}
