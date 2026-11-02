package operatorgates

import (
	"encoding/json"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/operator"
)

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
	diagnostics := (operatorevidence.Summary{UndecidedMetrics: m.UndecidedMetrics, PhaseDiagnostics: m.PhaseDiagnostics}).PublicDiagnostics()
	metrics := make([]operator.MetricDiagnostic, 0, len(diagnostics.UndecidedMetrics))
	for _, row := range diagnostics.UndecidedMetrics {
		metrics = append(metrics, operator.MetricDiagnostic{Phase: row.Phase, Event: row.Event, Metric: row.Metric, Causes: row.Causes})
	}
	phases := make([]operator.PhaseDiagnostic, 0, len(diagnostics.PhaseDiagnostics))
	for _, row := range diagnostics.PhaseDiagnostics {
		phases = append(phases, operator.PhaseDiagnostic{Gate: row.Gate, EventCount: row.EventCount, Phase: row.Phase, Included: row.Included, Cause: row.Cause, ExcludedEvents: row.ExcludedEvents, SaturationOverlaps: row.SaturationOverlaps, Issue: operator.EvidenceIssue{Path: row.Issue.Path, Reason: row.Issue.Reason}})
	}
	out := &operator.MediaDiagnostic{IncludedPhases: m.IncludedPhases, DroppedPhases: m.DroppedPhases, ExcludedEvents: m.ExcludedEvents, SaturationOverlaps: m.SaturationOverlaps, UndecidedMetrics: []operator.MetricDiagnostic{}, PhaseDiagnostics: []operator.PhaseDiagnostic{}, OmittedUndecidedMetrics: len(metrics), OmittedPhaseDiagnostics: len(phases), Status: status, ExpectedCalls: m.ExpectedCalls, CompletedCalls: m.CompletedCalls, FailedMetrics: m.FailedMetrics, InconclusiveMetrics: m.InconclusiveMetrics}
	// A final can contain six gates, each with both media and pool projections.
	// Reserve at most 12 KiB per projection inside the unchanged 256 KiB frame.
	// Keep phase causes first and make every omitted diagnostic row explicit.
	fits := func() bool { raw, _ := json.Marshal(out); return len(raw) <= 12<<10 }
	for _, row := range phases {
		out.PhaseDiagnostics = append(out.PhaseDiagnostics, row)
		out.OmittedPhaseDiagnostics--
		if !fits() {
			out.PhaseDiagnostics = out.PhaseDiagnostics[:len(out.PhaseDiagnostics)-1]
			out.OmittedPhaseDiagnostics++
			break
		}
	}
	for _, row := range metrics {
		out.UndecidedMetrics = append(out.UndecidedMetrics, row)
		out.OmittedUndecidedMetrics--
		if !fits() {
			out.UndecidedMetrics = out.UndecidedMetrics[:len(out.UndecidedMetrics)-1]
			out.OmittedUndecidedMetrics++
			break
		}
	}
	return out
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
