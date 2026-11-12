package main

import (
	"github.com/relaishq/relais/internal/loadgen"
)

// excludedPool reports the takeovers the pool left out for a wide kill bracket.
// CountedBackIn is the pooled status when they are judged with their brackets
// trusted. OwnerSignOff marks a pass held back because an excluded takeover's
// non-timing checks did not all pass.
type excludedPool struct {
	Takeovers     []excludedTakeover `json:"takeovers"`
	CountedBackIn string             `json:"countedBackIn"`
	OwnerSignOff  bool               `json:"ownerSignOff"`
}

// excludedTakeover is one excluded takeover's own results from the
// counted-back-in judgment. Only fixed names, outcomes and numbers leave.
type excludedTakeover struct {
	Phase           int              `json:"phase"`
	Event           int              `json:"event"`
	Calls           []excludedCall   `json:"calls"`
	Metrics         []excludedMetric `json:"metrics"`
	NonTimingPassed bool             `json:"nonTimingPassed"`
}
type excludedCall struct {
	WindowOutcome string         `json:"windowOutcome"`
	KillToLive    durationBounds `json:"killToLive"`
	EventGaps     []trackGap     `json:"eventGaps"`
}
type durationBounds struct {
	LowerNS int64 `json:"lowerNs"`
	UpperNS int64 `json:"upperNs"`
}
type trackGap struct {
	Kind string `json:"kind"`
	durationBounds
}
type excludedMetric struct {
	Metric  string `json:"metric"`
	Outcome string `json:"outcome"`
}

type eventPosition struct{ phase, event int }

// holdsPass reports a pooled pass that needs owner sign-off: an excluded
// takeover without its row, or one whose non-timing checks did not all pass.
func (e *excludedPool) holdsPass(kills int) bool {
	held := len(e.Takeovers) != kills
	for _, row := range e.Takeovers {
		held = held || !row.NonTimingPassed
	}
	return held
}

// killTimedMetric lists the checks that depend on the kill time, which an
// excluded takeover's wide bracket does not fix: operatorevidence.TimingMetric's
// three, plus resume and first_live, which are durations measured from the
// kill. It is wider than TimingMetric on purpose and applies only to excluded
// rows; rehearsal allowances keep TimingMetric. Every other metric is a
// non-timing check.
func killTimedMetric(metric string) bool {
	switch metric {
	case "first_content", "first_new_content", "distributed_window", "resume", "first_live":
		return true
	}
	return false
}

// soleExcludedCause reports an event whose only clock cause is the confirmed
// wide kill bracket. Only such a takeover leaves the pool.
func soleExcludedCause(e loadgen.EventResult, causes []string) bool {
	return e.Event.Kind == "kill" && len(e.Identities) > 0 && len(causes) == 1 && causes[0] == "clock-coverage-kill-excluded"
}

// excludedTakeoverRow reads pooled event k of a judged report.
func excludedTakeoverRow(r *loadgen.RunReport, k int, at eventPosition) excludedTakeover {
	row := excludedTakeover{Phase: at.phase, Event: at.event, Calls: []excludedCall{}, Metrics: []excludedMetric{}, NonTimingPassed: true}
	for _, m := range r.Yardstick {
		if m.EventIndex != k {
			continue
		}
		row.Metrics = append(row.Metrics, excludedMetric{m.Metric, m.Outcome})
		if !killTimedMetric(m.Metric) && m.Outcome != "pass" {
			row.NonTimingPassed = false
		}
	}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				continue
			}
			for _, m := range c.Call.Moves {
				d := m.Distributed
				if d == nil || !loadgen.MatchesEvent(r.Events[k], c.Call, m) {
					continue
				}
				call := excludedCall{WindowOutcome: "inconclusive", EventGaps: []trackGap{}}
				if d.WindowOutcome == "pass" || d.WindowOutcome == "fail" {
					call.WindowOutcome = d.WindowOutcome
				}
				if d.KillToLive != nil {
					call.KillToLive = durationBounds{int64(d.KillToLive.Lower), int64(d.KillToLive.Upper)}
				}
				for _, g := range d.Gaps {
					kind := "unclassified"
					if g.Kind == "audio" || g.Kind == "video" {
						kind = g.Kind
					}
					call.EventGaps = append(call.EventGaps, trackGap{kind, durationBounds{int64(g.EventGap.Lower), int64(g.EventGap.Upper)}})
				}
				row.Calls = append(row.Calls, call)
			}
		}
	}
	return row
}
