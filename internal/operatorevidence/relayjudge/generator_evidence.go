package relayjudge

import "strings"

// GeneratorEvidence contains no process, session or wall-clock identity. Checks
// aggregate violations by fixed rule name; EventOverlaps counts samples that
// also intersect an event window (including the maximum-lateness reach).
// Sample violations outside events are allowed within the time-weighted budget.
type GeneratorEvidence struct {
	Samples          int              `json:"samples"`
	InlineSamples    int              `json:"inlineSamples"`
	MediaNS          int64            `json:"mediaNs"`
	FlaggedNS        int64            `json:"flaggedNs"`
	FlaggedPercent   float64          `json:"flaggedPercent"`
	ClockExcursionNS int64            `json:"clockExcursionNs"`
	Checks           []GeneratorCheck `json:"checks"`
}

type GeneratorCheck struct {
	Rule          string  `json:"rule"`
	Observed      float64 `json:"observed"`
	Limit         float64 `json:"limit"`
	Count         int     `json:"count"`
	EventOverlaps int     `json:"eventOverlaps"`
}

func (g *GeneratorEvidence) check(rule string, observed, limit float64, overlap bool) {
	for i := range g.Checks {
		c := &g.Checks[i]
		if c.Rule == rule {
			c.Count++
			if strings.HasSuffix(rule, "-min") {
				c.Observed = min(c.Observed, observed)
			} else {
				c.Observed = max(c.Observed, observed)
			}
			if overlap {
				c.EventOverlaps++
			}
			return
		}
	}
	c := GeneratorCheck{Rule: rule, Observed: observed, Limit: limit, Count: 1}
	if overlap {
		c.EventOverlaps = 1
	}
	g.Checks = append(g.Checks, c)
}

// GeneratorRuleAllowed is the public report vocabulary, independent of input
// errors and sample contents. A new diagnosis must be explicitly admitted here.
func GeneratorRuleAllowed(rule string) bool {
	switch rule {
	case "generator-samples-min", "generator-sample-invalid", "generator-sample-sum-overflow", "generator-inline-invalid", "generator-sample-join", "generator-packets-min", "generator-call-span-invalid", "generator-event-span-invalid", "generator-numbers-invalid", "generator-timestamps-ordered", "generator-clock-delta-ns-max", "generator-clock-sum-overflow", "generator-clock-excursion-ns-max", "generator-cadence-ns-min", "generator-regular-samples-min", "generator-start-coverage-ns-max", "generator-end-coverage-ns-max", "generator-event-start-coverage-ns-max", "generator-event-end-coverage-ns-max", "generator-cpu-percent-max", "generator-p99-ns-max", "generator-max-ns-max", "generator-skipped-percent-max", "generator-interval-ns-max", "generator-event-overlaps-max", "generator-flagged-percent-max":
		return true
	}
	return false
}
