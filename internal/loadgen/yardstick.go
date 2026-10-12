package loadgen

import (
	"fmt"
	"sort"
	"time"

	"github.com/relais/pkg/callharness"
)

// MetricRow retains the existing per-call verdicts for one scheduled event.
// Coverage uses all affected calls, including missing records. A trusted
// failure still fails its metric even when generator validity is inconclusive.
type MetricRow struct {
	EventIndex      int           `json:"event_index"`
	After           time.Duration `json:"after_ns"`
	Scenario        string        `json:"scenario"`
	Metric          string        `json:"metric"`
	Events          int           `json:"events"`
	Trusted         int           `json:"trusted"`
	Failed          int           `json:"failed"`
	TrustedCoverage float64       `json:"trusted_coverage"`
	Outcome         string        `json:"outcome"`
}

// MatchesEvent ties a recorded move to one scheduled action.
func MatchesEvent(e EventResult, report *callharness.Report, move callharness.MoveReport) bool {
	if len(e.Identities) > 0 {
		if move.Identity == nil || !move.Identity.Valid() {
			return false
		}
		for _, id := range e.Identities {
			if id.Valid() && id == *move.Identity {
				return (e.Event.Kind == "kill" && move.Kind == "takeover") || ((e.Event.Kind == "move" || e.Event.Kind == "drain") && move.Kind == "move")
			}
		}
		return false
	}
	if report.Distributed != nil || move.Distributed != nil {
		return false
	}
	at := report.StartedAt.Add(move.Start)
	if e.Event.Kind == "kill" {
		delta := at.Sub(e.At)
		return move.Kind == "takeover" && move.From == e.Event.Worker && delta >= -time.Microsecond && delta <= time.Microsecond
	}
	target := move.To == e.Event.Worker
	if e.Event.Kind == "drain" {
		target = move.From == e.Event.Worker
	}
	return move.Kind == "move" && target && !at.Before(e.At) && !at.After(e.FinishedAt)
}

func yardstick(processes []ProcessReport, events []EventResult) ([]MetricRow, []string) {
	var result []MetricRow
	var causes []string
	pooled, pooledCauses, distributed := distributedTrials(processes)
	if distributed && pooled == "inconclusive" {
		causes = append(causes, pooledCauses...)
	}
	for i := range events {
		e := &events[i]
		e.Recorded = 0
		expected := map[string]bool{}
		for _, id := range e.Sessions {
			expected[id] = true
		}
		if len(expected) != e.Affected {
			causes = append(causes, fmt.Sprintf("event %d (%s at %s): %d distinct session IDs for %d affected calls", i+1, e.Event.Kind, e.Event.After, len(expected), e.Affected))
		}
		kind := e.Event.Kind
		if kind == "drain" {
			kind = "move"
		}
		if kind == "kill" {
			kind = "takeover"
		}
		rows := map[string]*MetricRow{}
		row := func(metric string) *MetricRow {
			if rows[metric] == nil {
				rows[metric] = &MetricRow{EventIndex: i, After: e.Event.After, Scenario: kind, Metric: metric, Events: e.Affected}
			}
			return rows[metric]
		}
		for _, name := range []string{"audio_loss", "audio_freshness", "resume"} {
			row(name)
		}
		seen := map[string]bool{}
		for _, p := range processes {
			for _, c := range p.Calls {
				if c.Call == nil || !expected[c.Session] {
					continue
				}
				var matching []callharness.MoveReport
				for _, move := range c.Call.Moves {
					if MatchesEvent(*e, c.Call, move) {
						matching = append(matching, move)
					}
				}
				if len(matching) == 0 {
					continue
				}
				if seen[c.Session] || len(matching) > 1 {
					causes = append(causes, fmt.Sprintf("event %d: duplicate matching records for session %s", i+1, c.Session))
				}
				if seen[c.Session] {
					continue
				}
				seen[c.Session] = true
				e.Recorded++
				m := matching[0].Measurement
				if matching[0].Distributed != nil {
					d := matching[0].Distributed
					v := callharness.MetricVerdict{Trusted: pooled != "inconclusive", Failed: pooled == "fail"}
					r := row("distributed_window")
					if v.Trusted {
						r.Trusted++
						if v.Failed {
							r.Failed++
						}
					}
					if matching[0].Measurement.Inconclusive {
						causes = append(causes, fmt.Sprintf("event %d session %s: content classification not proven across clock interval", i+1, c.Session))
					}
					for _, cause := range windowCauses(d) {
						causes = append(causes, fmt.Sprintf("event %d session %s: %s", i+1, c.Session, cause))
					}
				}
				verdicts := map[string]callharness.MetricVerdict{"audio_loss": m.AudioLoss, "audio_freshness": m.Audio.Verdict, "resume": m.ResumeVerdict}
				if m.VideoExpected {
					verdicts["video_loss"] = m.VideoLoss
					verdicts["video_freshness"] = m.Video.Verdict
					verdicts["first_content"] = m.FirstContentVerdict
					verdicts["first_new_content"] = m.FirstNewContentVerdict
					verdicts["first_live"] = m.FirstLiveVerdict
				}
				for name, v := range verdicts {
					r := row(name)
					if v.Trusted {
						r.Trusted++
						if v.Failed {
							r.Failed++
						}
					}
				}
			}
		}
		if e.Recorded != e.Affected {
			causes = append(causes, fmt.Sprintf("event %d (%s at %s): matching records for %d/%d affected calls; missing event evidence", i+1, e.Event.Kind, e.Event.After, e.Recorded, e.Affected))
		}
		keys := make([]string, 0, len(rows))
		for key := range rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			r := rows[key]
			if r.Events > 0 {
				r.TrustedCoverage = float64(r.Trusted) / float64(r.Events)
			}
			r.Outcome = "pass"
			if r.Events == 0 || r.TrustedCoverage < .8 {
				r.Outcome = "inconclusive"
			}
			if r.Failed > 0 {
				r.Outcome = "fail"
			}
			result = append(result, *r)
		}
	}
	return result, causes
}

// Pool every caller's trials for this run. Per-call summaries may lack the
// minimum trial counts even when the run supplies enough trials.
func distributedTrials(processes []ProcessReport) (string, []string, bool) {
	var moves []callharness.MoveReport
	var causes []string
	run := ""
	present := false
	for _, p := range processes {
		for _, c := range p.Calls {
			if c.Call == nil || c.Call.Distributed == nil {
				continue
			}
			present = true
			d := c.Call.Distributed
			if run == "" {
				run = d.RunID
			} else if run != d.RunID {
				causes = append(causes, "mixed distributed run identities")
			}
			if d.ClockError != "" {
				causes = append(causes, d.ClockError)
			}
			if len(d.Missing) > 0 {
				causes = append(causes, "armed distributed receipt missing")
			}
			moves = append(moves, c.Call.Moves...)
		}
	}
	if !present {
		return "", nil, false
	}
	outcome, judged := callharness.JudgeDistributedTrials(moves)
	if len(causes) > 0 && outcome != "fail" {
		outcome = "inconclusive"
	}
	return outcome, append(causes, judged...), true
}
func windowCauses(d *callharness.DistributedMeasurement) []string {
	if d.WindowOutcome == "pass" || d.WindowOutcome == "fail" {
		return nil
	}
	return d.Causes
}
