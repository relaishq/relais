package operatorevidence

import (
	"reflect"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/operator"
)

func PublicPhaseCause(s string) string {
	switch s {
	case "", "uncertain-receipt", "generator-guard", "generator saturation overlaps event window", "invalid-phase-evidence", "incomplete-relay-call", "qualification-skipped", "duplicate-operation-identity", "phase-run-differs", "caller-count-overflow":
		return s
	}
	return "unclassified"
}

// PublicDiagnostics copies diagnostic inventories and removes arbitrary labels.
func (p Summary) PublicDiagnostics() Summary {
	copy := p
	copy.UndecidedMetrics = append([]UndecidedMetric{}, p.UndecidedMetrics...)
	for i := range copy.UndecidedMetrics {
		row := &copy.UndecidedMetrics[i]
		row.Metric = PublicMetric(row.Metric)
		row.Causes = append([]string{}, row.Causes...)
		for j, cause := range row.Causes {
			row.Causes[j] = PublicTimingCause(cause)
		}
	}
	copy.PhaseDiagnostics = append([]PhaseDiagnostic{}, p.PhaseDiagnostics...)
	for i := range copy.PhaseDiagnostics {
		if !operator.Gate(copy.PhaseDiagnostics[i].Gate).Valid() {
			copy.PhaseDiagnostics[i].Gate = "unavailable"
		}
		copy.PhaseDiagnostics[i].Cause = PublicPhaseCause(copy.PhaseDiagnostics[i].Cause)
		copy.PhaseDiagnostics[i].Issue = copy.PhaseDiagnostics[i].Issue.Public(reflect.TypeFor[MediaInput](), reflect.TypeFor[loadgen.Message]())
	}
	return copy
}
