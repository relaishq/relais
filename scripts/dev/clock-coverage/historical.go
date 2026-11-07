package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type storedReport struct {
	HeadCaller   *storedReport            `json:"headCaller"`
	Clocks       []clocks.Evidence        `json:"clocks"`
	Bounds       clocks.Bounds            `json:"bounds"`
	Coverage     *clocks.CoverageEvidence `json:"clockCoverage"`
	CallerTiming []struct {
		Gate   string `json:"gate"`
		Events []struct {
			Kind        string `json:"kind"`
			StartUnixMS int64  `json:"startUnixMs"`
		} `json:"events"`
	} `json:"callerTiming"`
}

// Historical refreshes can repeat a log prefix. Merge equal-second envelopes
// conservatively: retain the largest error and every non-Normal leap state.
// beforeRun is not a filter; the shared evaluator decides temporal overlap.
func historicalTimelines(rows []clocks.Evidence) map[string]observe.CoverageTimeline {
	merged := map[string]map[int64]observe.CoverageRow{}
	breaks := map[string]map[observe.CoverageBreak]bool{}
	for _, sample := range rows {
		attempt := clocks.NewCoverageAttempt(sample)
		for slot, t := range attempt.Clocks {
			if merged[slot] == nil {
				merged[slot] = map[int64]observe.CoverageRow{}
				breaks[slot] = map[observe.CoverageBreak]bool{}
			}
			for _, r := range t.Rows {
				previous, exists := merged[slot][r.UnixSeconds]
				if exists {
					r.MaxErrorSeconds = max(r.MaxErrorSeconds, previous.MaxErrorSeconds)
					if previous.LeapStatus != "N" && (r.LeapStatus == "N" || previous.LeapStatus < r.LeapStatus) {
						r.LeapStatus = previous.LeapStatus
					}
				}
				merged[slot][r.UnixSeconds] = r
			}
			for _, b := range t.Breaks {
				breaks[slot][b] = true
			}
		}
	}
	out := map[string]observe.CoverageTimeline{}
	for slot, seconds := range merged {
		t := observe.CoverageTimeline{Rows: []observe.CoverageRow{}, Breaks: []observe.CoverageBreak{}}
		for _, r := range seconds {
			t.Rows = append(t.Rows, r)
		}
		sort.Slice(t.Rows, func(i, j int) bool { return t.Rows[i].UnixSeconds < t.Rows[j].UnixSeconds })
		for b := range breaks[slot] {
			t.Breaks = append(t.Breaks, b)
		}
		sort.Slice(t.Breaks, func(i, j int) bool {
			a, b := t.Breaks[i], t.Breaks[j]
			if a.FromUnixSeconds != b.FromUnixSeconds {
				return a.FromUnixSeconds < b.FromUnixSeconds
			}
			if a.UnixSeconds != b.UnixSeconds {
				return a.UnixSeconds < b.UnixSeconds
			}
			return a.Kind < b.Kind
		})
		out[slot] = t
	}
	return out
}

func eventKind(kind string) string {
	if kind == "takeover" {
		return "kill"
	}
	return kind
}

// Sanitized pools share one constant shift, but reports retain real UTC event
// starts at millisecond resolution. Intersect all matching timestamp buckets;
// retain the unresolved sub-millisecond width instead of inventing precision.
func alignHistoricalWindows(report storedReport, pool operatorevidence.Pool) ([]observe.CoverageWindow, int64, error) {
	lower, upper := int64(math.MinInt64), int64(math.MaxInt64)
	for _, phase := range pool.Phases {
		if len(phase.Events) == 0 {
			continue
		}
		matched := -1
		for i, timing := range report.CallerTiming {
			if timing.Gate != string(phase.Gate) || len(timing.Events) != len(phase.Events) {
				continue
			}
			if matched != -1 {
				return nil, 0, fmt.Errorf("ambiguous caller timing for clock alignment")
			}
			matched = i
		}
		if matched < 0 {
			return nil, 0, fmt.Errorf("report callerTiming required to align historical pool clocks")
		}
		for i, event := range phase.Events {
			timing := report.CallerTiming[matched].Events[i]
			if eventKind(timing.Kind) != eventKind(event.Event.Kind) || timing.StartUnixMS <= 0 || timing.StartUnixMS > math.MaxInt64/1000000 {
				return nil, 0, fmt.Errorf("event timing does not match historical pool")
			}
			diff, ok := observe.AddSigned(timing.StartUnixMS*1000000, -event.At.UnixNano())
			if !ok || diff > math.MaxInt64-999999 {
				return nil, 0, fmt.Errorf("clock alignment overflow")
			}
			lower = max(lower, diff)
			upper = min(upper, diff+999999)
		}
	}
	if lower > upper || lower == math.MinInt64 {
		return nil, 0, fmt.Errorf("historical event clocks have no common alignment")
	}
	windows := operatorevidence.CoverageWindows(pool)
	for i := range windows {
		w := &windows[i]
		event := pool.Phases[w.Phase].Events[w.Event]
		switch event.Event.Worker {
		case 1:
			w.Worker = "w1"
		case 2:
			w.Worker = "w2"
		default:
			return nil, 0, fmt.Errorf("historical event worker slot unavailable")
		}
		for _, s := range []*string{&w.StartUnixNS, &w.EndUnixNS} {
			n, ok := observe.CoverageNS(*s)
			if !ok {
				return nil, 0, fmt.Errorf("historical receipt timestamp unavailable")
			}
			n, ok = observe.AddSigned(n, lower)
			if !ok {
				return nil, 0, fmt.Errorf("historical receipt alignment overflow")
			}
			*s = fmt.Sprintf("%019d", n)
		}
	}
	return windows, upper - lower, nil
}

func windowTime(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "unavailable"
	}
	return time.Unix(0, n).UTC().Format(time.RFC3339Nano)
}
