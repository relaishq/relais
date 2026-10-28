package operatorrehearsal

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// CalibrationCheck records a breached rule, including tolerated flagged
// intervals. Causes identifies rejection; a flagged interval alone can pass.
// Rule names state the metric, unit and required minimum/maximum/equality.
type CalibrationCheck struct {
	Rule     string  `json:"rule"`
	Observed float64 `json:"observed"`
	Limit    float64 `json:"limit"`
}

func calibrationRule(s string) bool {
	switch s {
	case "generator-timestamps-ordered", "generator-clock-delta-ns-max", "generator-clock-excursion-ns-max", "generator-cadence-ns-min", "generator-regular-samples-min", "generator-media-span-covered", "generator-numbers-invalid", "generator-pacing-overflow", "generator-p99-ns-max", "generator-skipped-percent-max", "generator-cpu-percent-max", "generator-max-ns-max", "generator-interval-ns-max", "flagged-percent-max":
		return true
	}
	return false
}

func calibrationCause(s string) bool {
	if calibrationRule(s) {
		return true
	}
	switch s {
	case "calibration-judge-unavailable", "generator-evidence-rejected", "generator-event-exclusion", "caller-error", "known-media-failure", "incomplete-calibration-call", "generator-saturation-budget", "invalid-calibration-jsonl", "incomplete-calibration-evidence", "calibration-process-differs", "missing-calibration-stats", "calibration-caller-preparation-failed", "calibration-caller-evidence-failed", "calibration-evidence-rejected", "caller-stop-unconfirmed":
		return true
	}
	return false
}

func (c *Calibration) cause(s string) {
	if !slices.Contains(c.Causes, s) {
		c.Causes = append(c.Causes, s)
	}
}

func cleanCalibration(c Calibration) Calibration {
	c.MediaWindow.FlaggedIntervals = append([]CalibrationFlaggedInterval{}, c.MediaWindow.FlaggedIntervals...)
	if observe.Numbers(c.MediaWindow) != nil {
		c.MediaWindow = CalibrationMediaWindow{FlaggedIntervals: []CalibrationFlaggedInterval{}}
	}
	c.Causes = append([]string{}, c.Causes...)
	for i, s := range c.Causes {
		if !calibrationCause(s) {
			c.Causes[i] = "incomplete-calibration-evidence"
		}
	}
	if !c.Passed && len(c.Causes) == 0 {
		c.cause("incomplete-calibration-evidence")
	}
	c.Checks = append([]CalibrationCheck{}, c.Checks...)
	for i, check := range c.Checks {
		if !calibrationRule(check.Rule) || math.IsNaN(check.Observed) || math.IsInf(check.Observed, 0) || math.IsNaN(check.Limit) || math.IsInf(check.Limit, 0) {
			c.Checks[i] = CalibrationCheck{Rule: "incomplete-calibration-evidence"}
		}
	}
	return c
}

func (r *rehearsal) recordCalibration(stage string, err error) {
	c := &r.report.CPU.Calibration
	if err != nil {
		c.Passed = false
		c.cause(stage)
	}
	if !c.Passed {
		safe := cleanCalibration(*c)
		*c = safe
		raw, _ := json.Marshal(safe)
		_, _ = fmt.Fprintf(r.diagnostics, "calibration-rejected %s\n", raw)
	}
}
