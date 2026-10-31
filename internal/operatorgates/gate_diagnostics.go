package operatorgates

import "github.com/relaishq/relais/pkg/operator"

func diagnosticCode(reason string) string {
	switch reason {
	case "", "missing-live-call", "missing-live-inventory", "restore-unsettled", "restore-unconfirmed",
		"known-call-loss", "compatibility-refused", "known-relay-failure", "relay-self-fence", "relay-continuity-failed",
		"procedure-incomplete", "media-unavailable", "media-undecided",
		"pooled-qualification-unavailable", "pooled-qualification-undecided", "procedure-unqualified":
		return reason
	default:
		return ErrorCategory("Error", reason)
	}
}
func mediaDiagnostic(m *MediaSummary) *operator.MediaDiagnostic {
	if m == nil {
		return nil
	}
	status := m.Status
	if status != operator.Passed && status != operator.Failure && status != operator.Inconclusive {
		status = operator.Inconclusive
	}
	return &operator.MediaDiagnostic{Status: status, ExpectedCalls: m.ExpectedCalls, CompletedCalls: m.CompletedCalls, FailedMetrics: m.FailedMetrics, InconclusiveMetrics: m.InconclusiveMetrics}
}
func (s *Session) retainGateDiagnostics(result *SequenceResult, stopped map[operator.Gate]bool) {
	rows := make([]operator.GateDiagnostic, 0, len(result.Gates))
	for i, g := range result.Gates {
		p := result.Procedures[i]
		reason := p.ProcedureReason
		if reason == "" && !p.Completed {
			reason = p.Reason
			if reason == "" {
				reason = "procedure-incomplete"
			}
		}
		gateReason := g.Reason
		if g.Status == operator.Inconclusive && gateReason == "" {
			switch {
			case !p.Completed:
				gateReason = "procedure-incomplete"
			case p.AwaitingPool && result.Pool == nil:
				gateReason = "pooled-qualification-unavailable"
			case p.AwaitingPool && result.Pool.Status == operator.Inconclusive:
				gateReason = "pooled-qualification-undecided"
			case p.Media == nil:
				gateReason = "media-unavailable"
			case p.Media.Status == operator.Inconclusive:
				gateReason = "media-undecided"
			default:
				gateReason = "procedure-unqualified"
			}
		}
		rows = append(rows, operator.GateDiagnostic{Gate: g.Gate, Reason: diagnosticCode(gateReason), Procedure: operator.ProcedureDiagnostic{Completed: p.Completed, Reason: diagnosticCode(reason), CompletedSteps: p.Process.CompletedSteps}, Media: mediaDiagnostic(p.Media), Pool: mediaDiagnostic(result.Pool), PoolPending: p.AwaitingPool && (result.Pool == nil || result.Pool.Status == operator.Inconclusive), CallerStopped: stopped[g.Gate]})
	}
	s.mu.Lock()
	s.diagnostics = rows
	s.mu.Unlock()
}
