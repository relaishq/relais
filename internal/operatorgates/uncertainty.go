package operatorgates

import (
	"slices"
	"strconv"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func (s *Session) RecordUncertainty(u operator.Uncertainty) {
	u = u.Public()
	s.uncertainties = append(s.uncertainties, u)
	if s.uncertaintySink != nil {
		s.uncertaintySink(u)
	}
}
func (s *Session) unknownScope(kind, target, action string, end time.Duration, rule string) {
	s.RecordUncertainty(operator.Uncertainty{Kind: kind, RequestID: "0", Target: target, Action: action, DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (end - s.clock.Elapsed()).Milliseconds(), Rule: rule})
}
func (s *Session) SetUncertaintySink(sink func(operator.Uncertainty)) { s.uncertaintySink = sink }
func (s *Session) Uncertainties() []operator.Uncertainty              { return slices.Clone(s.uncertainties) }
func (s *Session) unknownRequest(id, rule string) {
	p := s.requests[id]
	if p == nil {
		s.RecordUncertainty(operator.Uncertainty{Kind: "session", RequestID: "0", Target: "session", Action: "latch", Rule: rule})
		return
	}
	kind := string(p.operation)
	if p.operation == operator.HostExecute && p.action == "status" {
		kind = "status.read"
	}
	s.RecordUncertainty(operator.Uncertainty{Kind: kind, RequestID: id, Target: p.target, Action: p.action, DeadlineUnixMS: strconv.FormatInt(s.clock.Wall().Add(p.end-s.clock.Elapsed()).UnixMilli(), 10), RemainingMS: (p.end - s.clock.Elapsed()).Milliseconds(), Rule: rule})
}
