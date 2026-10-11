package loadgen

import (
	"github.com/relais/pkg/callharness"
	"sort"
)

// MetricRow aggregates the existing yardstick without changing its decisions.
// Any trusted failure stays failed. Passing needs >=80% trusted event coverage.
// Generator saturation overrides the run's validity, never these raw rows.
type MetricRow struct {
	Scenario        string  `json:"scenario"`
	Metric          string  `json:"metric"`
	Events          int     `json:"events"`
	Trusted         int     `json:"trusted"`
	Failed          int     `json:"failed"`
	TrustedCoverage float64 `json:"trusted_coverage"`
	Outcome         string  `json:"outcome"`
}

func yardstick(processes []ProcessReport) []MetricRow {
	rows := map[string]*MetricRow{}
	for _, p := range processes {
		for _, c := range p.Calls {
			if c.Call == nil {
				continue
			}
			for _, move := range c.Call.Moves {
				m := move.Measurement
				verdicts := map[string]callharness.MetricVerdict{"audio_loss": m.AudioLoss, "audio_freshness": m.Audio.Verdict, "resume": m.ResumeVerdict}
				if m.VideoExpected {
					verdicts["video_loss"] = m.VideoLoss
					verdicts["video_freshness"] = m.Video.Verdict
					verdicts["first_content"] = m.FirstContentVerdict
					verdicts["first_new_content"] = m.FirstNewContentVerdict
					verdicts["first_live"] = m.FirstLiveVerdict
				}
				for name, v := range verdicts {
					key := move.Kind + ":" + name
					r := rows[key]
					if r == nil {
						r = &MetricRow{Scenario: move.Kind, Metric: name}
						rows[key] = r
					}
					r.Events++
					if v.Trusted {
						r.Trusted++
						if v.Failed {
							r.Failed++
						}
					}
				}
			}
		}
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]MetricRow, 0, len(keys))
	for _, key := range keys {
		r := rows[key]
		r.TrustedCoverage = float64(r.Trusted) / float64(r.Events)
		r.Outcome = "pass"
		if r.TrustedCoverage < .8 {
			r.Outcome = "inconclusive"
		}
		if r.Failed > 0 {
			r.Outcome = "fail"
		}
		result = append(result, *r)
	}
	return result
}
