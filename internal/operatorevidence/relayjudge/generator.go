package relayjudge

import (
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// Keep the existing judge's one-second cadence, 25ms wall/monotonic allowance,
// 1% time-weighted saturation budget and independent maximum-lateness guard.
const samplePeriod = time.Second
const sampleTolerance = 25 * time.Millisecond

func readSample(raw []byte, pid int) (loadgen.Stats, bool) {
	var m loadgen.Message
	if observe.DecodeEvidenceLimit(raw, &m, operator.MaxBlobBytes) != nil || observe.Numbers(m) != nil || m.Type != "stats" || m.PID != pid || m.Stats == nil {
		return loadgen.Stats{}, false
	}
	s := *m.Stats
	_, intervalOK := observe.AddCount(int64(s.IntervalPacing.SentFrames), int64(s.IntervalPacing.SkippedFrames))
	_, allOK := observe.AddCount(int64(s.Pacing.SentFrames), int64(s.Pacing.SkippedFrames))
	return s, !s.At.IsZero() && s.Interval > 0 && intervalOK && allOK
}
func sampleChecks(s loadgen.Stats) []GeneratorCheck {
	l := loadgen.DefaultLimits()
	var checks []GeneratorCheck
	for _, c := range []GeneratorCheck{
		{Rule: "generator-cpu-percent-max", Observed: s.CPUPerGOMAXPROCS, Limit: l.CPUPercent},
		{Rule: "generator-p99-ns-max", Observed: float64(s.IntervalLateness.P99), Limit: float64(l.P99)},
		{Rule: "generator-max-ns-max", Observed: float64(s.IntervalLateness.Max), Limit: float64(500 * time.Millisecond)},
		{Rule: "generator-skipped-percent-max", Observed: s.IntervalPacing.SkippedPercent(), Limit: l.SkippedPercent},
		{Rule: "generator-interval-ns-max", Observed: float64(s.Interval), Limit: float64(samplePeriod + sampleTolerance)},
	} {
		if c.Observed > c.Limit {
			checks = append(checks, c)
		}
	}
	return checks
}

type wallWindow struct{ start, end time.Time }

func qualifyGenerator(in Input, calls map[int]message, g *GeneratorEvidence) bool {
	g.Samples = len(in.Samples)
	if len(in.Samples) == 0 {
		g.check("generator-samples-min", 0, 1, false)
		return false
	}
	var collected, inline []loadgen.Stats
	var sampleDuration, sampleRequests int64
	for _, raw := range in.Samples {
		s, ok := readSample(raw, in.PID)
		if !ok {
			g.check("generator-sample-invalid", 1, 0, false)
			return false
		}
		var okDuration, okRequests bool
		sampleDuration, okDuration = observe.AddCount(sampleDuration, int64(s.Interval))
		sampleRequests, okRequests = observe.AddCount(sampleRequests, int64(s.IntervalStatusRequests))
		if !okDuration || !okRequests {
			g.check("generator-sample-sum-overflow", 1, 0, false)
			return false
		}
		collected = append(collected, s)
	}
	d := json.NewDecoder(strings.NewReader(in.CallerJSONL))
	for {
		var raw json.RawMessage
		err := d.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			g.check("generator-inline-invalid", 1, 0, false)
			return false
		}
		var header struct {
			Type string `json:"type"`
		}
		if observe.DecodeLimit(raw, &header, nil, operator.MaxBlobBytes) != nil {
			g.check("generator-inline-invalid", 1, 0, false)
			return false
		}
		if header.Type == "stats" {
			s, ok := readSample(raw, in.PID)
			if !ok {
				g.check("generator-inline-invalid", 1, 0, false)
				return false
			}
			inline = append(inline, s)
		}
	}
	g.InlineSamples = len(inline)
	if collected[len(collected)-1].Lateness.Packets == 0 {
		g.check("generator-packets-min", 0, 1, false)
		return false
	}
	if !reflect.DeepEqual(collected, inline) {
		g.check("generator-sample-join", 0, 1, false)
		return false
	}
	var start, end time.Time
	var windows []wallWindow
	for _, m := range calls {
		c := m.Call
		if c == nil || c.StartedAt == nil || c.StartedAt.IsZero() || !duration(c.FirstVideoSentAt) || *c.FirstVideoSentAt <= 0 || !duration(c.HungUpAt) || *c.HungUpAt <= *c.FirstVideoSentAt || c.SentVideo == nil || !positive(c.SentVideo.Frames) {
			g.check("generator-call-span-invalid", 1, 0, false)
			return false
		}
		a, b := c.StartedAt.Add(*c.FirstVideoSentAt), c.StartedAt.Add(*c.HungUpAt)
		if start.IsZero() || a.Before(start) {
			start = a
		}
		if b.After(end) {
			end = b
		}
		for _, e := range m.Episodes {
			if !duration(e.Last) || !duration(e.Start) || !duration(e.Video) || !duration(e.Audio) {
				g.check("generator-event-span-invalid", 1, 0, false)
				return false
			}
			first := c.StartedAt.Add(*e.Last).Add(-samplePeriod)
			last := c.StartedAt.Add(min(*c.HungUpAt, max(*e.Video, *e.Audio))).Add(samplePeriod)
			if *e.Video == 0 || *e.Audio == 0 {
				last = b
			}
			windows = append(windows, wallWindow{first, last})
		}
	}
	return generatorWindowEvidence(collected, start, end, windows, g)
}
func generatorWindow(samples []loadgen.Stats, start, end time.Time, windows []wallWindow) bool {
	return generatorWindowEvidence(samples, start, end, windows, &GeneratorEvidence{})
}
func generatorWindowEvidence(samples []loadgen.Stats, start, end time.Time, windows []wallWindow, g *GeneratorEvidence) bool {
	if len(samples) == 0 || start.IsZero() || !end.After(start) || observe.Numbers(samples) != nil {
		g.check("generator-numbers-invalid", 1, 0, false)
		return false
	}
	g.MediaNS = int64(end.Sub(start))
	regular := false
	var cumulative, excursion time.Duration
	for i, s := range samples {
		if i > 0 {
			if !s.At.After(samples[i-1].At) {
				g.check("generator-timestamps-ordered", 0, 1, false)
				return false
			}
			delta := s.At.Sub(samples[i-1].At) - s.Interval
			if delta < -sampleTolerance || delta > sampleTolerance {
				g.check("generator-clock-delta-ns-max", float64(max(delta, -delta)), float64(sampleTolerance), false)
				return false
			}
			sum, ok := observe.AddSigned(int64(cumulative), int64(delta))
			if !ok {
				g.check("generator-clock-sum-overflow", 1, 0, false)
				return false
			}
			cumulative = time.Duration(sum)
			if cumulative < -sampleTolerance || cumulative > sampleTolerance {
				g.check("generator-clock-excursion-ns-max", float64(max(cumulative, -cumulative)), float64(sampleTolerance), false)
				return false
			}
			excursion = max(excursion, cumulative, -cumulative)
		}
		if s.Interval >= samplePeriod-sampleTolerance && s.Interval <= samplePeriod+sampleTolerance {
			regular = true
		}
		if i > 0 && i < len(samples)-1 && s.Interval < samplePeriod-sampleTolerance && samples[i-1].Interval <= samplePeriod+sampleTolerance && samples[i+1].Interval <= samplePeriod+sampleTolerance {
			g.check("generator-cadence-ns-min", float64(s.Interval), float64(samplePeriod-sampleTolerance), false)
			return false
		}
	}
	g.ClockExcursionNS = int64(excursion)
	if !regular {
		g.check("generator-regular-samples-min", 0, 1, false)
		return false
	}
	if gap := samples[0].At.Add(-samples[0].Interval).Sub(start); gap > min(samples[0].Interval, 1500*time.Millisecond) {
		g.check("generator-start-coverage-ns-max", float64(gap), float64(min(samples[0].Interval, 1500*time.Millisecond)), false)
		return false
	}
	if samples[len(samples)-1].At.Before(end) {
		g.check("generator-end-coverage-ns-min", 0, float64(end.Sub(samples[len(samples)-1].At)), false)
		return false
	}
	// Match statsClockExcursion: the largest absolute cumulative excursion
	// over the whole timeline pads both event edges, including later reversals.
	padded := make([]wallWindow, 0, len(windows))
	for _, w := range windows {
		w = wallWindow{w.start.Add(-excursion), w.end.Add(excursion)}
		padded = append(padded, w)
		if w.end.Before(w.start) {
			g.check("generator-event-span-invalid", 1, 0, false)
			return false
		}
		if gap := samples[0].At.Add(-samples[0].Interval).Sub(w.start); gap > 0 {
			g.check("generator-event-start-coverage-ns-max", float64(gap), 0, false)
			return false
		}
		if gap := w.end.Sub(samples[len(samples)-1].At); gap > 0 {
			g.check("generator-event-end-coverage-ns-min", 0, float64(gap), false)
			return false
		}
	}

	var intervals []wallWindow
	overlaps := 0
	for _, s := range samples {
		checks := sampleChecks(s)
		if len(checks) == 0 {
			continue
		}
		a, b := s.At.Add(-s.Interval), s.At
		overlap := false
		for _, w := range padded {
			if a.Add(-s.IntervalLateness.Max).Before(w.end) && b.After(w.start) {
				overlap = true
				break
			}
		}
		if overlap {
			overlaps++
		}
		for _, c := range checks {
			g.check(c.Rule, c.Observed, c.Limit, overlap)
		}
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		if b.After(a) {
			intervals = append(intervals, wallWindow{a, b})
		}
	}
	// End timestamps are ordered, but short catch-up intervals and the allowed
	// wall/monotonic skew can reverse starts. Match the existing union policy.
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	var weighted time.Duration
	var until time.Time
	for _, interval := range intervals {
		a, b := interval.start, interval.end
		if a.Before(until) {
			a = until
		}
		if b.After(a) {
			weighted += b.Sub(a)
		}
		if b.After(until) {
			until = b
		}
	}
	g.FlaggedNS = int64(weighted)
	g.FlaggedPercent = 100 * weighted.Seconds() / end.Sub(start).Seconds()
	if overlaps > 0 {
		g.check("generator-event-overlaps-max", float64(overlaps), 0, false)
	}
	if g.FlaggedPercent > 1 {
		g.check("generator-flagged-percent-max", g.FlaggedPercent, 1, false)
	}
	return overlaps == 0 && g.FlaggedPercent <= 1
}
