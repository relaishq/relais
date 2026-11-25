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
func flagged(s loadgen.Stats) bool {
	l := loadgen.DefaultLimits()
	return s.CPUPerGOMAXPROCS > l.CPUPercent || s.IntervalLateness.P99 > l.P99 || s.IntervalLateness.Max > 500*time.Millisecond || s.IntervalPacing.SkippedPercent() > l.SkippedPercent || s.Interval > samplePeriod+sampleTolerance
}

type wallWindow struct{ start, end time.Time }

func qualifyGenerator(in Input, calls map[int]message) bool {
	if len(in.Samples) == 0 {
		return false
	}
	var collected, inline []loadgen.Stats
	var sampleDuration, sampleRequests int64
	for _, raw := range in.Samples {
		s, ok := readSample(raw, in.PID)
		if !ok {
			return false
		}
		var okDuration, okRequests bool
		sampleDuration, okDuration = observe.AddCount(sampleDuration, int64(s.Interval))
		sampleRequests, okRequests = observe.AddCount(sampleRequests, int64(s.IntervalStatusRequests))
		if !okDuration || !okRequests {
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
			return false
		}
		var header struct {
			Type string `json:"type"`
		}
		if observe.DecodeLimit(raw, &header, nil, operator.MaxBlobBytes) != nil {
			return false
		}
		if header.Type == "stats" {
			s, ok := readSample(raw, in.PID)
			if !ok {
				return false
			}
			inline = append(inline, s)
		}
	}
	if collected[len(collected)-1].Lateness.Packets == 0 || !reflect.DeepEqual(collected, inline) {
		return false
	}
	var start, end time.Time
	var windows []wallWindow
	for _, m := range calls {
		c := m.Call
		if c == nil || c.StartedAt == nil || c.StartedAt.IsZero() || !duration(c.FirstVideoSentAt) || *c.FirstVideoSentAt <= 0 || !duration(c.HungUpAt) || *c.HungUpAt <= *c.FirstVideoSentAt || c.SentVideo == nil || !positive(c.SentVideo.Frames) {
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
	return generatorWindow(collected, start, end, windows)
}
func generatorWindow(samples []loadgen.Stats, start, end time.Time, windows []wallWindow) bool {
	if len(samples) == 0 || start.IsZero() || !end.After(start) || observe.Numbers(samples) != nil {
		return false
	}
	regular := false
	var cumulative time.Duration
	for i, s := range samples {
		if i > 0 {
			if !s.At.After(samples[i-1].At) {
				return false
			}
			delta := s.At.Sub(samples[i-1].At) - s.Interval
			if delta < -sampleTolerance || delta > sampleTolerance {
				return false
			}
			sum, ok := observe.AddSigned(int64(cumulative), int64(delta))
			if !ok {
				return false
			}
			cumulative = time.Duration(sum)
			if cumulative < -sampleTolerance || cumulative > sampleTolerance {
				return false
			}
		}
		if s.Interval >= samplePeriod-sampleTolerance && s.Interval <= samplePeriod+sampleTolerance {
			regular = true
		}
		if i > 0 && i < len(samples)-1 && s.Interval < samplePeriod-sampleTolerance && samples[i-1].Interval <= samplePeriod+sampleTolerance && samples[i+1].Interval <= samplePeriod+sampleTolerance {
			return false
		}
	}
	if !regular || samples[0].At.Add(-samples[0].Interval).After(start.Add(min(samples[0].Interval, 1500*time.Millisecond))) || samples[len(samples)-1].At.Before(end) {
		return false
	}
	for _, w := range windows {
		if w.start.Before(samples[0].At.Add(-samples[0].Interval)) || w.end.After(samples[len(samples)-1].At) || w.end.Before(w.start) {
			return false
		}
	}
	var intervals []wallWindow
	for _, s := range samples {
		if !flagged(s) {
			continue
		}
		a, b := s.At.Add(-s.Interval), s.At
		for _, w := range windows {
			if a.Add(-s.IntervalLateness.Max).Before(w.end) && b.After(w.start) {
				return false
			}
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
	return 100*weighted.Seconds()/end.Sub(start).Seconds() <= 1
}
