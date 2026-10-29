// Package operatorgates owns a session's effects, pending identities and fatal
// latch. Request exchanges are serialized; concurrent evidence collection and
// fence notifications share the state lock. Adapters never own this state.
package operatorgates

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type pending struct {
	operation      operator.Operation
	target, action string
	end            time.Duration
	effect         bool
	expired        bool
	terminal       bool
	outcome        operator.Certainty
}
type Session struct {
	mu              sync.Mutex
	exchangeMu      sync.Mutex
	ID              string
	Budget          Budget
	clock           Clock
	capabilities    map[operator.Operation]bool
	next            uint64
	requests        map[string]*pending
	latched         bool
	code            string
	trace           []operator.Trace
	firstError      *operator.Trace
	gates           []operator.Gate
	activeEnd       *time.Duration
	digest          string
	scenario        operator.Scenario
	spool           *os.Root
	uncertainties   []operator.Uncertainty
	uncertaintySink func(operator.Uncertainty)

	observationFailureSink func(string, string, error)
}

func NewSession(id string, init operator.Init, clock Clock) (*Session, error) {
	if id == "" || len(id) > 128 || !digestPattern.MatchString(init.Scenario.Digest) || init.Scenario.SchemaVersion != 1 || len(init.Scenario.Gates) == 0 || len(init.Scenario.Gates) > 6 {
		return nil, errors.New("invalid-init")
	}
	seen := map[operator.Gate]bool{}
	for _, g := range init.Scenario.Gates {
		if !g.Valid() || seen[g] {
			return nil, errors.New("invalid-scenario")
		}
		seen[g] = true
	}
	budget, err := NewBudget(clock, init.ArmedAtUnixMS, init.EndUnixMS)
	if err != nil {
		return nil, err
	}
	s := &Session{digest: init.Scenario.Digest, ID: id, Budget: budget, clock: clock, capabilities: map[operator.Operation]bool{}, requests: map[string]*pending{}, gates: append([]operator.Gate{}, init.Scenario.Gates...)}
	s.scenario = init.Scenario
	s.scenario.Gates = slices.Clone(init.Scenario.Gates)
	for _, op := range init.Capabilities {
		s.capabilities[op] = true
	}
	return s, nil
}
func effectful(op operator.Operation, action string) bool {
	switch op {
	case operator.MailboxRead, operator.CallerReady, operator.StoreObserve:
		return false
	case operator.HostExecute:
		switch action {
		case "store-status", "worker-status", "relay-status", "relay-inspect", "worker-empty", "inventory", "clock":
			return false
		}
	}
	return true
}
func (s *Session) record(event, id string, c operator.Certainty, code string) {
	entry := operator.Trace{Event: event, RequestID: id, Certainty: c, Code: code, AtUnixMS: strconv.FormatInt(s.clock.Wall().UnixMilli(), 10)}
	s.trace = append(s.trace, entry)
	if s.firstError == nil && (c == operator.Failed || c == operator.Unknown) {
		s.firstError = &entry
	}
}
func (s *Session) Halt(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halt(code)
}
func (s *Session) halt(code string) {
	code = ErrorCategory("Error", code)
	if !s.latched {
		if code == "command-result-unknown" && len(s.uncertainties) == 0 {
			s.unknownRequest("", "unattributed-latch")
		}
		s.latched = true
		s.code = code
		s.record("latch", "", operator.Unknown, code)
	}
}
func (s *Session) Latched() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.latched }

// Issue records identity before the writer can submit. The caller sends this
// request exactly once, then calls Accept or TransportLost. No retry API exists.
func (s *Session) Issue(op operator.Operation, target, action string, args json.RawMessage, script []byte, end time.Duration) (operator.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if s.latched {
		return operator.Request{}, errors.New("session-latched")
	}
	if len(s.requests) >= 32768 {
		return operator.Request{}, errors.New("request-limit")
	}
	if !s.capabilities[op] {
		return operator.Request{}, errors.New("unsupported-capability")
	}
	if len(script) > operator.MaxScriptBytes || len(args) > operator.MaxFrameBytes/2 || (len(args) > 0 && !json.Valid(args)) {
		return operator.Request{}, errors.New("invalid-request")
	}
	end = min(end, s.Budget.end)
	if s.activeEnd != nil {
		end = min(end, *s.activeEnd)
	}
	if end <= s.clock.Elapsed() {
		return operator.Request{}, errors.New("gate-deadline")
	}
	effect := effectful(op, action)
	for _, p := range s.requests {
		if !p.terminal && p.effect && effect {
			return operator.Request{}, errors.New("effect-pending")
		}
	}
	s.next++
	deadline, remaining := s.Budget.Wire(end)
	r := operator.Request{ID: strconv.FormatUint(s.next, 10), Operation: op, Target: target, Action: action, Args: bytes.Clone(args), Script: bytes.Clone(script), DeadlineUnixMS: deadline, RemainingMS: remaining}
	s.requests[r.ID] = &pending{end: end, effect: effect, operation: op, target: target, action: action}
	s.record("request", r.ID, "", "")
	return r, nil
}

// Accept uses an operation-specific classification from public gate code, not
// an adapter's gate verdict. Transport uncertainty always dominates evidence.
func (s *Session) Accept(session string, r operator.Reply, evidence operator.Certainty) (operator.Certainty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.requests[r.ID]
	if session != s.ID || p == nil || !r.Transport.Valid() || !evidence.Valid() {
		s.unknownRequest(r.ID, "malformed-reply")
		s.halt("protocol-error")
		return operator.Unknown, errors.New("protocol-error")
	}
	if p.terminal {
		s.unknownRequest(r.ID, "malformed-reply")
		s.halt("duplicate-result")
		return operator.Unknown, errors.New("duplicate-result")
	}
	outcome := evidence
	if r.Transport == operator.Unknown {
		outcome = operator.Unknown
	}
	if !r.NotSubmitted && (r.ExitCode == nil || *r.ExitCode != 0 || r.Transport != operator.Succeeded) {
		if p.effect || r.Transport == operator.Unknown {
			outcome = operator.Unknown
		} else {
			outcome = operator.Failed
		}
	}
	if r.NotSubmitted {
		if r.Transport != operator.Failed || !refusalCode(r.RefusalCode) {
			s.halt("invalid-refusal")
			return operator.Unknown, errors.New("invalid-refusal")
		}
		outcome = operator.Failed
	}
	// Timeout/late success is never evidence that the deadline was met.
	if p.expired || s.clock.Elapsed() >= p.end {
		if outcome != operator.Failed {
			outcome = operator.Unknown
		}
		p.expired = true
	}
	p.terminal = true
	p.outcome = outcome
	if outcome == operator.Unknown {
		rule := "operation-evidence-unknown"
		switch {
		case p.expired:
			rule = "timeout"
		case r.UncertaintyRule != "":
			rule = r.UncertaintyRule
		case r.Transport == operator.Unknown:
			rule = "transport-unknown"
		case r.ExitCode == nil:
			rule = "missing-exit-status"
		case *r.ExitCode != 0:
			rule = "unconfirmed-exit"
		}
		s.unknownRequest(r.ID, rule)
		s.halt("command-result-unknown")
	}
	s.record("result", r.ID, outcome, "")
	return outcome, nil
}
func (s *Session) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
}
func (s *Session) expire() {
	for _, id := range s.requestIDs() {
		p := s.requests[id]
		if !p.terminal && !p.expired && s.clock.Elapsed() >= p.end {
			p.expired = true
			p.outcome = operator.Unknown
			s.unknownRequest(id, "timeout")
			s.halt("command-result-unknown")
			s.record("timeout", id, operator.Unknown, "")
		}
	}
}
func (s *Session) TransportLost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transportLost()
}
func (s *Session) transportLost() {
	for _, id := range s.requestIDs() {
		if !s.requests[id].terminal {
			noted := false
			for _, u := range s.uncertainties {
				if u.RequestID == id && u.Target == s.requests[id].target {
					noted = true
					break
				}
			}
			if !noted {
				s.unknownRequest(id, "lost-reply")
			}
		}
	}
	s.halt("command-result-unknown")
}
func (s *Session) Final(gates []operator.GateResult) operator.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	r := operator.Result{SchemaVersion: 1, ScenarioDigest: s.digest, Status: operator.Passed, Complete: true, Latched: s.latched, Pending: []string{}, Gates: gates, Trace: append([]operator.Trace{}, s.trace...), FirstError: s.firstError}
	r.Uncertainties = slices.Clone(s.uncertainties)
	for id, p := range s.requests {
		if !p.terminal {
			r.Pending = append(r.Pending, id)
		}
		if p.expired || p.outcome == operator.Unknown {
			r.Status = operator.Inconclusive
			r.Complete = false
		}
	}
	sort.Strings(r.Pending)
	if s.latched || len(r.Pending) > 0 || len(gates) != len(s.gates) {
		r.Status = operator.Inconclusive
		r.Complete = false
	}
	r.Gates = append([]operator.GateResult{}, gates...)
	for i, g := range gates {
		if g.Reason != "" {
			r.Gates[i].Reason = ErrorCategory("Error", g.Reason)
		}
		if !g.Gate.Valid() {
			r.Gates[i].Gate = ""
		}
		if i >= len(s.gates) || g.Gate != s.gates[i] {
			r.Status = operator.Inconclusive
			r.Complete = false
		}
		if g.Status != operator.Passed {
			r.Complete = false
			if r.Status != operator.Failure {
				r.Status = operator.Inconclusive
			}
		}
		if g.Status == operator.Failure {
			r.Status = operator.Failure
		}
	}
	return r
}

// CompleteGate applies the same absolute cutoff to the returned verdict. Known
// failure survives a late or unknown outcome; an unknown stops the sequence.
func (s *Session) CompleteGate(r operator.GateResult, end time.Duration) operator.GateResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeEnd != nil {
		end = min(end, *s.activeEnd)
	}
	defer func() { s.activeEnd = nil }()
	if s.clock.Elapsed() >= end && r.Status != operator.Failure {
		r.Status = operator.Inconclusive
		r.Reason = "gate-deadline"
	}
	switch r.Status {
	case operator.Passed, operator.Failure, operator.Inconclusive, operator.NotRun:
	default:
		r.Status = operator.Inconclusive
		r.Reason = "command-result-unknown"
	}
	if r.Reason == "command-result-unknown" || r.Reason == "gate-deadline" || r.Reason == "interrupted" || r.Status == operator.Failure {
		if r.Reason == "command-result-unknown" && len(s.uncertainties) == 0 {
			s.recordUncertainty(operator.Uncertainty{Kind: "gate", RequestID: "0", Target: "session", Action: string(r.Gate), DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (end - s.clock.Elapsed()).Milliseconds(), Rule: "operation-evidence-unknown"})
		}
		s.halt(r.Reason)
	}
	return r
}

func refusalCode(code string) bool {
	switch code {
	case "unsupported-operation", "invalid-target", "invalid-request", "deadline-before-submission":
		return true
	}
	return false
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SetBlobRoot binds the already opened local spool. Its path is never an artifact.
func (s *Session) SetBlobRoot(root *os.Root) { s.spool = root }

// BeginGate fixes this gate's cutoff once. Asking again cannot renew it.
func (s *Session) BeginGate() (time.Duration, bool) {
	return s.beginGate(s.clock.Elapsed())
}
func (s *Session) beginGate(now time.Duration) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeEnd == nil {
		end, ok := s.Budget.gateEnd(now)
		if !ok {
			return end, false
		}
		s.activeEnd = &end
	}
	return *s.activeEnd, now < *s.activeEnd && !s.latched
}
func (s *Session) requestIDs() []string {
	ids := make([]string, 0, len(s.requests))
	for id, p := range s.requests {
		if !p.terminal {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) < len(ids[j])
		}
		return ids[i] < ids[j]
	})
	return ids
}
