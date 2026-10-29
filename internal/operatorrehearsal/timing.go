package operatorrehearsal

import (
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

type TimingUndecided struct {
	Gate operator.Gate `json:"gate"`
	operatorevidence.UndecidedMetric
}

// Free rehearsal eligibility is separate from the unchanged strict judge verdict.
func rehearsalMedia(p *operatorevidence.Summary) bool {
	if p == nil || observe.Numbers(p) != nil || !p.CompleteMedia || p.Status != operator.Passed && p.Status != operator.Inconclusive || p.FailedMetrics != 0 || p.LostCalls != 0 || p.DecryptionFailures != 0 || p.ICERestarts != 0 || p.ExcludedEvents != 0 || p.DroppedPhases != 0 || p.DuplicatePackets != 0 || p.SaturationOverlaps != 0 || p.ExpectedCalls != 4 || p.CompletedCalls != 4 || p.RelayCalls != 1 || p.IncludedPhases != 4 || p.EventCount < 1 || p.MetricCount < 1 || p.InconclusiveMetrics > p.MetricCount || len(p.PhaseDiagnostics) != 4 {
		return false
	}
	events := 0
	for i, phase := range p.PhaseDiagnostics {
		if phase.Phase != i || phase.Gate != string(CoveredGates[i]) || !phase.Included || phase.Cause != "" || phase.Issue.Path != "" || phase.Issue.Reason != "" || phase.ExcludedEvents != 0 || phase.SaturationOverlaps != 0 || i < 3 && phase.EventCount < 1 || i == 3 && phase.EventCount != 0 {
			return false
		}
		events += phase.EventCount
	}
	if events != p.EventCount || len(p.UndecidedMetrics) != p.InconclusiveMetrics || (p.Status == operator.Passed) != (p.InconclusiveMetrics == 0) {
		return false
	}
	type key struct {
		phase, event int
		metric       string
	}
	seen := map[key]bool{}
	for _, row := range p.UndecidedMetrics {
		k := key{row.Phase, row.Event, row.Metric}
		if !operatorevidence.TimingMetric(row.Metric) || row.Phase < 0 || row.Phase >= 3 || row.Event < 0 || row.Event >= p.PhaseDiagnostics[row.Phase].EventCount || len(row.Causes) == 0 || seen[k] {
			return false
		}
		seen[k] = true
		for _, cause := range row.Causes {
			if cause == "unclassified" || operatorevidence.PublicTimingCause(cause) != cause {
				return false
			}
		}
	}
	return true
}
