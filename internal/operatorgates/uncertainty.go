package operatorgates

import (
	"slices"
	"strconv"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func (s *Session) RecordUncertainty(u operator.Uncertainty) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordUncertainty(u)
}
func (s *Session) recordUncertainty(u operator.Uncertainty) {
	u = u.Public()
	s.uncertainties = append(s.uncertainties, u)
	if s.uncertaintySink != nil {
		s.uncertaintySink(u.Public())
	}
}
func (s *Session) unknownScope(kind, target, action string, end time.Duration, rule string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordUncertainty(operator.Uncertainty{Kind: kind, RequestID: "0", Target: target, Action: action, DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (end - s.clock.Elapsed()).Milliseconds(), Rule: rule})
}
func (s *Session) SetUncertaintySink(sink func(operator.Uncertainty)) { s.uncertaintySink = sink }
func (s *Session) Uncertainties() []operator.Uncertainty {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := slices.Clone(s.uncertainties)
	for i := range rows {
		rows[i] = rows[i].Public()
	}
	return rows
}
func (s *Session) unknownRequest(id, rule string) {
	p := s.requests[id]
	if p == nil {
		s.recordUncertainty(operator.Uncertainty{Kind: "session", RequestID: "0", Target: "session", Action: "latch", Rule: rule})
		return
	}
	kind := string(p.operation)
	if p.operation == operator.HostExecute && p.action == "status" {
		kind = "status.read"
	}
	s.recordUncertainty(operator.Uncertainty{Kind: kind, RequestID: id, Target: p.target, Action: p.action, DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(p.end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (p.end - s.clock.Elapsed()).Milliseconds(), Rule: rule, Code: p.failureCode, ElapsedMS: p.failureElapsedMS, Phase: p.failurePhase, StepDeadlineMS: p.remainingMS})
}

// Evidence decorates the existing pending identity before Accept records its
// uncertainty. It cannot classify the result, clear the latch or submit work.
func (s *Session) hostFailureEvidence(id, code, phase string, elapsed *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.requests[id]
	p.failureCode, p.failurePhase, p.failureElapsedMS = code, phase, elapsed
}

func (s *Session) noteUnknownRequest(id, rule string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknownRequest(id, rule)
}

// A child keeps the batch's recorded request identity and deadline, while its
// fixed host slot and allow-listed reason identify the failed observation.
func (s *Session) noteUnknownHost(id, slot, action, rule string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.requests[id]
	s.recordUncertainty(operator.Uncertainty{Kind: string(operator.HostExecute), RequestID: id, Target: slot, Action: action, DeadlineUnixMS: p.deadlineUnixMS, RemainingMS: (p.end - s.clock.Elapsed()).Milliseconds(), Rule: rule})
}
