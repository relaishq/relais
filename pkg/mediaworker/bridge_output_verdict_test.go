package mediaworker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These verdicts run after sampling. They never change the fixture's schedule,
// callbacks, admission or observation work.
func bridgeOutputPhasesVerdict(phases map[string]any) string {
	if len(phases) == 0 {
		return "inconclusive"
	}
	verdict := "meets"
	for _, raw := range phases {
		phase, ok := raw.(map[string]any)
		if !ok {
			return "inconclusive"
		}
		lower, lowerOK := phase["lower_ms"].([]float64)
		upper, upperOK := phase["upper_ms"].([]float64)
		if !lowerOK || !upperOK || len(lower) != 3 || len(upper) != 3 {
			return "inconclusive"
		}
		if lower[1] > 150 {
			return "misses"
		}
		if upper[1] > 150 {
			verdict = "inconclusive"
		}
	}
	return verdict
}

func TestBridgeOutputPhasesCannotHideRapidInterruptionMiss(t *testing.T) {
	phase := func(lower, upper float64) map[string]any {
		return map[string]any{"lower_ms": []float64{0, lower, lower}, "upper_ms": []float64{0, upper, upper}}
	}
	for _, tc := range []struct {
		name   string
		phases map[string]any
		want   string
	}{
		{"all-fit", map[string]any{"rotation": phase(90, 96), "rapid": phase(118, 123)}, "meets"},
		// Actual campaign shape: whole-run upper p95 128 ms, rapid lower p95 196 ms.
		{"aggregate-masks-miss", map[string]any{"rotation": phase(119, 125), "rapid": phase(196, 202)}, "misses"},
		{"observer-uncertainty", map[string]any{"rotation": phase(119, 125), "rapid": phase(149, 151)}, "inconclusive"},
		{"no-samples", nil, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, bridgeOutputPhasesVerdict(tc.phases)) })
	}
}
