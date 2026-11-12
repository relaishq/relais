package main

import (
	"slices"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

// Owner decision 2026-11-11 (#446, option C). A takeover's first decoded
// content is labelled by comparing caller times with the kill time, which
// comes from another clock. The pinned caller evaluates every kill time in the
// clock interval. When the label differs between them, it leaves the verdict
// undecided, even if every kill time passes.

// boundaryMetric is one takeover verdict that carries a content label.
type boundaryMetric struct {
	name    string
	verdict *callharness.MetricVerdict
}

func boundaryMetrics(m *callharness.MoveReport) []boundaryMetric {
	return []boundaryMetric{{"first_content", &m.Measurement.FirstContentVerdict}, {"first_new_content", &m.Measurement.FirstNewContentVerdict}}
}

// onlyLabelDiffers reports a verdict whose every evaluated kill time passed.
// The caller joins the reasons of each kill time that is untrusted or failed,
// then adds the interval reason and the duration bounds. A passing kill time
// has no reason. So these two reasons alone prove that only the label differs.
// The fixed reason codes also match sanitized evidence.
func onlyLabelDiffers(v callharness.MetricVerdict) bool {
	if len(v.Reasons) == 0 || len(v.Reasons) > 2 || timingCause(v.Reasons[0]) != boundaryCauses[0] {
		return false
	}
	return len(v.Reasons) == 1 || timingCause(v.Reasons[1]) == boundaryCauses[1]
}

var boundaryCauses = []string{"classification-varies-with-clock-interval", "duration-bounds-uncertain"}

// settleBoundaryTakeovers trusts each takeover verdict that passes for every
// kill time. The verdict keeps its reasons as the boundary label. The window
// passes again only when every content verdict is trusted and the window has
// no cause but those reasons. Run it before the product judge and clock
// qualification.
func settleBoundaryTakeovers(r *loadgen.RunReport) {
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				continue
			}
			for i := range c.Call.Moves {
				settleBoundaryTakeover(&c.Call.Moves[i])
			}
		}
	}
}

func settleBoundaryTakeover(m *callharness.MoveReport) {
	if m.Kind != "takeover" {
		return
	}
	settled := false
	for _, metric := range boundaryMetrics(m) {
		if !metric.verdict.Trusted && onlyLabelDiffers(*metric.verdict) {
			metric.verdict.Trusted = true
			settled = true
		}
	}
	if !settled {
		return
	}
	v := &m.Measurement
	v.Inconclusive = false
	for _, verdict := range []callharness.MetricVerdict{v.AudioLoss, v.VideoLoss, v.Audio.Verdict, v.Video.Verdict, v.ResumeVerdict, v.FirstContentVerdict, v.FirstNewContentVerdict, v.FirstLiveVerdict} {
		v.Inconclusive = v.Inconclusive || !verdict.Trusted
	}
	// callCollections requires the mapped window record of every move. A gap
	// that does not pass leaves a cause of its own there.
	d := m.Distributed
	if v.Inconclusive || d.WindowOutcome != "inconclusive" {
		return
	}
	for _, cause := range d.Causes {
		if !slices.Contains(boundaryCauses, timingCause(cause)) {
			return
		}
	}
	d.WindowOutcome, d.Outcome = "pass", "pass"
}

// boundaryRows lists each settled verdict that is still trusted after clock
// qualification, once per event and metric, with its fixed reason codes.
func boundaryRows(r *loadgen.RunReport) []undecidedMetric {
	var out []undecidedMetric
	for k, e := range r.Events {
		for _, p := range r.Processes {
			for _, c := range p.Calls {
				if c.Call == nil {
					continue
				}
				for i := range c.Call.Moves {
					m := &c.Call.Moves[i]
					if !loadgen.MatchesEvent(e, c.Call, *m) {
						continue
					}
					for _, metric := range boundaryMetrics(m) {
						if !metric.verdict.Trusted || !onlyLabelDiffers(*metric.verdict) || slices.ContainsFunc(out, func(row undecidedMetric) bool { return row.Event == k && row.Metric == metric.name }) {
							continue
						}
						row := undecidedMetric{Event: k, Metric: metric.name, Causes: []string{}}
						for _, reason := range metric.verdict.Reasons {
							row.Causes = append(row.Causes, timingCause(reason))
						}
						out = append(out, row)
					}
				}
			}
		}
	}
	return out
}

// excludedBoundaryRows keeps the boundary label of each excluded takeover.
// back is the counted-back-in judgment, the only one that settles an excluded
// takeover; its rows already carry the original phase and event.
func excludedBoundaryRows(back previewMediaSummary) []undecidedMetric {
	var out []undecidedMetric
	for _, row := range back.BoundaryMetrics {
		if slices.ContainsFunc(back.Excluded.Takeovers, func(e excludedTakeover) bool { return e.Phase == row.Phase && e.Event == row.Event }) {
			out = append(out, row)
		}
	}
	return out
}
