package main

import (
	"slices"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// Diagnostics only: consume the yardstick already judged by the pinned product.
// Neither these rows nor their reason codes participate in the strict verdict.
type undecidedMetric struct {
	Phase  int      `json:"phase"`
	Event  int      `json:"event"`
	Metric string   `json:"metric"`
	Causes []string `json:"causes"`
}

func timingCause(reason string) string { return observe.TimingCause(reason) }

func undecidedRows(r *loadgen.RunReport) []undecidedMetric {
	out := []undecidedMetric{}
	poolCauses := poolOnlyCauses(r)
	for _, row := range r.Yardstick {
		if row.Outcome != "inconclusive" {
			continue
		}
		d := undecidedMetric{Event: row.EventIndex, Metric: row.Metric, Causes: []string{}}
		add := func(reasons []string) {
			for _, reason := range reasons {
				code := timingCause(reason)
				if !slices.Contains(d.Causes, code) {
					d.Causes = append(d.Causes, code)
				}
			}
		}
		if row.EventIndex >= 0 && row.EventIndex < len(r.Events) {
			for _, p := range r.Processes {
				for _, c := range p.Calls {
					if c.Call == nil {
						continue
					}
					for _, m := range c.Call.Moves {
						if !loadgen.MatchesEvent(r.Events[row.EventIndex], c.Call, m) {
							continue
						}
						switch row.Metric {
						case "distributed_window":
							if m.Distributed != nil && m.Distributed.WindowOutcome != "pass" && m.Distributed.WindowOutcome != "fail" {
								add(m.Distributed.Causes)
							}
						case "first_content":
							add(m.Measurement.FirstContentVerdict.Reasons)
						case "first_new_content":
							add(m.Measurement.FirstNewContentVerdict.Reasons)
						}
					}
				}
			}
		}
		if row.Metric == "distributed_window" && len(d.Causes) == 0 {
			d.Causes = append(d.Causes, "metric-undecided")
			add(poolCauses)
		}
		if len(d.Causes) == 0 {
			d.Causes = append(d.Causes, "trusted-coverage-below-80-percent")
		}
		slices.Sort(d.Causes)
		out = append(out, d)
	}
	return out
}

// JudgeDistributedTrials appends each non-passing window's causes and one
// window-status reason. Subtract those occurrences, retaining pool-owned
// reasons even when their text also appears in a window's cause list.
func poolOnlyCauses(r *loadgen.RunReport) []string {
	if r.Distributed == nil {
		return nil
	}
	windowCauses := map[string]int{}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil || c.Call.Distributed == nil {
				continue
			}
			for _, m := range c.Call.Moves {
				d := m.Distributed
				if d == nil {
					continue
				}
				window := d.WindowOutcome
				if window == "" {
					window = d.Outcome
				}
				if window == "pass" {
					continue
				}
				if window == "fail" {
					windowCauses["server loss or caller content failure"]++
				} else {
					windowCauses["event window evidence incomplete"]++
				}
				for _, cause := range d.Causes {
					windowCauses[cause]++
				}
			}
		}
	}
	var causes []string
	for _, cause := range r.Distributed.Causes {
		if windowCauses[cause] > 0 {
			windowCauses[cause]--
		} else {
			causes = append(causes, cause)
		}
	}
	return causes
}

func lostCallCount(input *previewMediaInput, r *loadgen.RunReport) int {
	expected, lost := map[string]bool{}, map[string]bool{}
	for _, session := range input.Sessions {
		expected[session] = true
	}
	for _, receipt := range input.Receipts {
		if receipt.Identity.Valid() && receipt.Identity.RunID == input.RunID && expected[receipt.Identity.SessionID] && receipt.Result.Lost {
			lost[receipt.Identity.SessionID] = true
		}
	}
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			if c.Call == nil || !expected[c.Session] || c.Call.Distributed == nil || c.Call.Distributed.RunID != input.RunID {
				continue
			}
			for _, m := range c.Call.Moves {
				if m.Lost {
					lost[c.Session] = true
				}
			}
		}
	}
	return len(lost)
}
