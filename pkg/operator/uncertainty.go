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
	Code           string `json:"code,omitempty"`
	ElapsedMS      *int64 `json:"elapsedMs,omitempty"`
	Phase          string `json:"phase,omitempty"`
	// StepDeadlineMS is the original request budget, not the remaining budget
	// at failure. Retain it so Public can recheck elapsed time in saved reports.
	StepDeadlineMS int64 `json:"stepDeadlineMs,omitempty"`
	// HTTPStatus and Codes describe a definite HTTP error reply to a host
	// step. The step result stays unknown: the server may have acted first.
	HTTPStatus int      `json:"httpStatus,omitempty"`
	Codes      []string `json:"codes,omitempty"`
}

var publicCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

const maxPublicCodes = 4

type UncertaintyRecorder interface{ RecordUncertainty(Uncertainty) }

// Public is also applied when reading a report, so adapter text cannot enter
// these fields through a manually constructed diagnostic.
func (u Uncertainty) Public() Uncertainty {
	if u.Rule == "caller-stopped" && u.Code != "" {
		u.Code = PublicCallerStopReason(u.Code)
	}
	if !publicCode.MatchString(u.Code) {
		u.Code = ""
	}
	if u.ElapsedMS != nil && (*u.ElapsedMS < 0 || u.StepDeadlineMS <= 0 || *u.ElapsedMS > u.StepDeadlineMS) {
		u.ElapsedMS = nil
	}
	if u.ElapsedMS == nil {
		u.StepDeadlineMS = 0
	} else {
		elapsed := *u.ElapsedMS
		u.ElapsedMS = &elapsed
	}
	if u.Action != "move" || u.Phase != "status" && u.Phase != "post" {
		u.Phase = ""
	}
	if u.HTTPStatus < 100 || u.HTTPStatus > 599 {
		u.HTTPStatus = 0
	}
	// A fresh slice: the result never aliases the caller's codes.
	var codes []string
	for _, code := range u.Codes {
		if len(codes) < maxPublicCodes && publicCode.MatchString(code) {
			codes = append(codes, code)
		}
	}
	u.Codes = codes
	switch u.Kind {
	case "host.lease-read", "host.lease-read-batch", "host.read-batch", "host.execute", "status.read", "caller.ready", "caller.start", "caller.stop", "caller.feed", "mailbox.read", "mailbox.create", "caller.collect", "judge", "store.observe", "store.failover", "session", "gate", "gate.prepare":
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
	case "hosts", "relay", "relay-two", "w1", "w2", "shared", "caller", "store", "judge", "session":
	default:
		u.Target = "unavailable"
	}
	switch u.Action {
	case "", "ready", "start", "stop", "collect", "latch", "judge", "observe", "failover", "caller-token.json", "transport.json", "setup-ready.json", "begin.json", "ready.json", "registration.json", "caller-probes", "caller-start", "ack", "hangup", "inventory", "clock", "telemetry", "signal-stop", "signal-start", "move", "status", "store-status", "worker-status", "relay-status", "relay-inspect", "worker-empty", "freeze", "unfreeze", "drain", "worker-stop", "replace", "worker-start", "control-stop", "control-start", "worker-down", "relay-stage", "relay-stop", "relay-replenish", "relay-isolate", "relay-restore":
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

// PublicCallerStopReason admits only the bridge's fixed caller stop codes.
func PublicCallerStopReason(reason string) string {
	switch reason {
	case "generator-saturation-hard-ceiling", "generator-saturation-sustained", "generator-saturation-budget", "invalid-generator-statistics", "caller-stream-failure", "caller-supervision-failure":
		return reason
	}
	return "unavailable"
}
