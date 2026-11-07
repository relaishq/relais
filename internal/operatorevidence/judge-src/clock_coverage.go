package main

import (
	"slices"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func coverageForPhase(input *previewPoolInput, phase int) map[int][]string {
	out := map[int][]string{}
	for event := range input.Phases[phase].Events {
		causes := input.ClockCoverage.WindowCauses(phase, event, input.PreCoverageEvidence)
		out[event] = causes
	}
	return out
}

// Each pinned release first judges its own media policy. Qualification removes
// clock trust, retains independent failures and populations, then asks that same
// release to rebuild its yardstick, distributed pool and run outcome.
func qualifyClockCoverage(r *loadgen.RunReport, coverage map[int][]string) {
	changed := false
	for event := range r.Events {
		causes := coverage[event]
		if len(causes) == 0 {
			continue
		}
		changed = true
		for pi := range r.Processes {
			for ci := range r.Processes[pi].Calls {
				c := r.Processes[pi].Calls[ci].Call
				if c == nil {
					continue
				}
				for mi := range c.Moves {
					m := &c.Moves[mi]
					if !loadgen.MatchesEvent(r.Events[event], c, *m) {
						continue
					}
					if d := m.Distributed; d != nil {
						if d.WindowOutcome == "fail" || d.WindowOutcome == "" && d.Outcome == "fail" {
							d.WindowOutcome, d.Outcome = "fail", "fail"
						} else {
							d.WindowOutcome = "inconclusive"
							if d.Outcome != "fail" {
								d.Outcome = "inconclusive"
							}
						}
						d.Causes = appendUnique(d.Causes, causes)
						qualifyClockMetric(&d.AudioLoss, causes)
						qualifyClockMetric(&d.VideoLoss, causes)
					}
					v := &m.Measurement
					v.Inconclusive = false
					v.Failed = false
					for _, metric := range []*callharness.MetricVerdict{&v.AudioLoss, &v.VideoLoss, &v.Audio.Verdict, &v.Video.Verdict, &v.ResumeVerdict, &v.FirstContentVerdict, &v.FirstNewContentVerdict, &v.FirstLiveVerdict} {
						qualifyClockMetric(metric, causes)
						v.Inconclusive = v.Inconclusive || !metric.Trusted
						v.Failed = v.Failed || metric.Trusted && metric.Failed
					}
					v.Reasons = appendUnique(v.Reasons, causes)
				}
			}
		}
	}
	if changed {
		r.Judge()
	}
}
func appendUnique(dst, values []string) []string {
	for _, value := range values {
		if !slices.Contains(dst, value) {
			dst = append(dst, value)
		}
	}
	return dst
}

func qualifyClockMetric(metric *callharness.MetricVerdict, causes []string) {
	if !metric.Trusted || !metric.Failed {
		metric.Trusted, metric.Failed = false, false
	}
	metric.Reasons = appendUnique(metric.Reasons, causes)
}
