// Compiled only inside a temporary, pinned public-module snapshot by judge.ts.
package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
)

type receipt struct {
	Identity      callharness.OperationIdentity `json:"identity"`
	Kind          string                        `json:"kind"`
	Start         time.Time                     `json:"start"`
	End           time.Time                     `json:"end"`
	Result        controlplane.MoveResult       `json:"result"`
	Uncertain     bool                          `json:"uncertain"`
	Codes         []string                      `json:"codes"`
	ResponseError string                        `json:"responseError"`
	HTTPStatus    int                           `json:"httpStatus"`
}

// Public #116: loss proves failure; an operation error or uncertainty cannot.
func uncertainReceipt(r receipt) bool {
	for _, code := range r.Codes {
		if code == "uncertain" {
			return true
		}
	}
	return r.Uncertain || r.Result.Error != "" || r.ResponseError != "" || r.HTTPStatus >= 400 || r.Start.IsZero() || r.End.IsZero()
}

type flaggedInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
type callerSaturation struct {
	Checks         []generatorCheck  `json:"checks,omitempty"`
	Job            string            `json:"job"`
	FlaggedSeconds float64           `json:"flagged_seconds"`
	MediaSeconds   float64           `json:"media_seconds"`
	Percent        float64           `json:"percent"`
	Outcome        string            `json:"outcome"`
	Causes         []string          `json:"causes,omitempty"`
	Flagged        []flaggedInterval `json:"flagged,omitempty"`
}

// Retain the existing error text while exposing only fixed numeric diagnostics.
type generatorEvidenceError struct {
	text  string
	check generatorCheck
}

func (e *generatorEvidenceError) Error() string { return e.text }

type eventExclusion struct {
	Identity        callharness.OperationIdentity `json:"identity"`
	Job             string                        `json:"job"`
	OriginalOutcome string                        `json:"originalOutcome"`
	Outcome         string                        `json:"outcome"`
	Cause           string                        `json:"cause"`
	Start           time.Time                     `json:"windowStart"`
	End             time.Time                     `json:"windowEnd"`
}

func sampleFlagged(s loadgen.Stats) bool {
	return len(generatorSampleChecks(s)) > 0
}

// The predicate and its diagnostic projection share one set of comparisons.
func generatorSampleChecks(s loadgen.Stats) []generatorCheck {
	if observe.Numbers(s) != nil {
		return []generatorCheck{{Rule: "generator-numbers-invalid", Observed: 1, Limit: 0}}
	}
	if _, ok := observe.AddCount(int64(s.IntervalPacing.SentFrames), int64(s.IntervalPacing.SkippedFrames)); !ok {
		return []generatorCheck{{Rule: "generator-pacing-overflow", Observed: 1, Limit: 0}}
	}
	l := loadgen.DefaultLimits()
	// A sampler tick can split the pacer's first late send from its skipped-frame
	// record on the next 20–33ms tick. Flag Max independently of p99/skips:
	// 500ms is safely below ~0.96s (one sample period minus a pacer tick), so a
	// >=1s stall cannot leave its only useful backward reach in a clean sample.
	var failed []generatorCheck
	for _, check := range []generatorCheck{
		{"generator-cpu-percent-max", s.CPUPerGOMAXPROCS, l.CPUPercent},
		{"generator-p99-ns-max", float64(s.IntervalLateness.P99), float64(l.P99)},
		{"generator-max-ns-max", float64(s.IntervalLateness.Max), float64(500 * time.Millisecond)},
		{"generator-skipped-percent-max", s.IntervalPacing.SkippedPercent(), l.SkippedPercent},
		{"generator-interval-ns-max", float64(s.Interval), float64(expectedSamplePeriod + statsClockTolerance)},
	} {
		if check.Observed > check.Limit {
			failed = append(failed, check)
		}
	}
	return failed
}

type statsManifestEntry struct {
	Name  string `json:"name"`
	SHA   string `json:"sha256"`
	Bytes int    `json:"bytes"`
}

func loadSamples(dir string, pid int, verify bool) ([]loadgen.Stats, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "stats", "*.json"))
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("missing generator stats")
	}
	checks := map[string]statsManifestEntry{}
	if verify {
		raw, err := os.ReadFile(filepath.Join(dir, "stats-manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("missing collected stats manifest")
		}
		var entries []statsManifestEntry
		if err = observe.DecodeEvidenceLimit(raw, &entries, 64<<20); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if checks[e.Name].Name != "" {
				return nil, fmt.Errorf("duplicate stats manifest entry")
			}
			checks[e.Name] = e
		}
		if len(checks) != len(paths) {
			return nil, fmt.Errorf("incomplete stats manifest")
		}
	}
	var samples []loadgen.Stats
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if verify {
			e, ok := checks[filepath.Base(path)]
			sum := sha256.Sum256(raw)
			if !ok || e.Bytes != len(raw) || e.SHA != fmt.Sprintf("%x", sum) {
				return nil, fmt.Errorf("collected stats checksum differs")
			}
		}
		s, err := readSample(raw, pid)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		samples = append(samples, s)
	}
	if err := observe.Numbers(samples); err != nil {
		return nil, err
	}
	// Preserve mailbox sequence order: sorting by wall time could hide a backward clock step.
	for i := 1; i < len(samples); i++ {
		if !samples[i].At.After(samples[i-1].At) {
			return nil, &generatorEvidenceError{"duplicate or unordered stats timestamp", generatorCheck{"generator-timestamps-ordered", 0, 1}}
		}
		delta := samples[i].At.Sub(samples[i-1].At) - samples[i].Interval
		if delta < -statsClockTolerance || delta > statsClockTolerance {
			return nil, &generatorEvidenceError{fmt.Sprintf("stats wall/monotonic discontinuity at sample %d: %s", i, delta), generatorCheck{"generator-clock-delta-ns-max", math.Abs(float64(delta)), float64(statsClockTolerance)}}
		}
	}
	if excursion := statsClockExcursion(samples); excursion > statsClockTolerance {
		return nil, &generatorEvidenceError{fmt.Sprintf("cumulative stats wall/monotonic drift exceeds 25ms: %s", excursion), generatorCheck{"generator-clock-excursion-ns-max", float64(excursion), float64(statsClockTolerance)}}
	}
	// No period is serialized by this pinned public schema. Check the 1s
	// contract, not just interval-to-wall agreement. A short catch-up tick may
	// follow/precede a delayed (flagged) tick; the first/final samples may be partial.
	// Other short intervals, or a stream with no regular 1s tick, are unknown.
	regular := false
	for i, sample := range samples {
		if sample.Interval >= expectedSamplePeriod-statsClockTolerance && sample.Interval <= expectedSamplePeriod+statsClockTolerance {
			regular = true
		}
		if i > 0 && i < len(samples)-1 && sample.Interval < expectedSamplePeriod-statsClockTolerance && samples[i-1].Interval <= expectedSamplePeriod+statsClockTolerance && samples[i+1].Interval <= expectedSamplePeriod+statsClockTolerance {
			return nil, &generatorEvidenceError{fmt.Sprintf("stats sampling cadence inconsistent with 1s at sample %d", i), generatorCheck{"generator-cadence-ns-min", float64(sample.Interval), float64(expectedSamplePeriod - statsClockTolerance)}}
		}
	}
	if !regular {
		return nil, &generatorEvidenceError{"stats sampling period cannot be verified as 1s", generatorCheck{"generator-regular-samples-min", 0, 1}}
	}
	return samples, nil
}

// 25ms permits modest wall-clock slewing and timestamp capture jitter around the
// monotonic sampler interval, but rejects clock steps or missing wall-time periods.
// A flagged interval must still be anchored to the caller's event wall-clock window.
const statsClockTolerance = 25 * time.Millisecond
const expectedSamplePeriod = time.Second
const saturationPercentLimit = 1

// Conservatively use the largest absolute cumulative wall/monotonic excursion
// over the entire collected process timeline for both event edges. This also
// covers every observed excursion before either edge, including later reversals.
// Above 25ms the timeline is unknown; below it the guard never assumes zero drift.
func statsClockExcursion(samples []loadgen.Stats) time.Duration {
	if observe.Numbers(samples) != nil {
		return time.Duration(math.MaxInt64)
	}
	var cumulative, largest time.Duration
	for i := 1; i < len(samples); i++ {
		delta, ok := observe.AddSigned(int64(samples[i].At.Sub(samples[i-1].At)), -int64(samples[i].Interval))
		if !ok {
			return time.Duration(math.MaxInt64)
		}
		sum, ok := observe.AddSigned(int64(cumulative), delta)
		if !ok || sum == math.MinInt64 {
			return time.Duration(math.MaxInt64)
		}
		cumulative = time.Duration(sum)
		magnitude := cumulative
		if magnitude < 0 {
			magnitude = -magnitude
		}
		largest = max(largest, magnitude)
	}
	return largest
}

func sampleSpan(samples []loadgen.Stats, start, end time.Time) error {
	if observe.Numbers(struct {
		Samples    []loadgen.Stats
		Start, End time.Time
	}{samples, start, end}) != nil || len(samples) == 0 || start.IsZero() || !end.After(start) {
		return fmt.Errorf("missing caller stats span")
	}
	// Allow startup timestamp alignment by one sampler interval, capped at 1.5s.
	// This allowance never relaxes event-window coverage or changes the budget start.
	tolerance := min(samples[0].Interval, 1500*time.Millisecond)
	if samples[0].At.Add(-samples[0].Interval).After(start.Add(tolerance)) || samples[len(samples)-1].At.Before(end) {
		return &generatorEvidenceError{"stats do not cover caller media start through hangup", generatorCheck{"generator-media-span-covered", 0, 1}}
	}
	return nil
}
func saturationBudget(job string, samples []loadgen.Stats, start, end time.Time) callerSaturation {
	b := callerSaturation{Job: job, Outcome: "pass"}
	if observe.Numbers(struct {
		Samples    []loadgen.Stats
		Start, End time.Time
	}{samples, start, end}) != nil || start.IsZero() || !end.After(start) {
		b.Outcome = "inconclusive"
		b.Causes = append(b.Causes, "missing caller media wall-time")
		return b
	}
	b.MediaSeconds = end.Sub(start).Seconds()
	// Overlap expands backward by maximum pacer lateness: the pacer can report
	// a long stall only after the sampler woke first and emitted an unflagged tick.
	// Budget weighting stays on actual sample wall-time, without counting Max twice.
	var intervals []flaggedInterval
	for _, s := range samples {
		if sampleFlagged(s) {
			f := flaggedInterval{s.At.Add(-s.Interval), s.At}
			b.Flagged = append(b.Flagged, flaggedInterval{f.Start.Add(-s.IntervalLateness.Max), f.End})
			if f.Start.Before(start) {
				f.Start = start
			}
			if f.End.After(end) {
				f.End = end
			}
			if f.End.After(f.Start) {
				intervals = append(intervals, f)
			}
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].Start.Before(intervals[j].Start) })
	var until time.Time
	for _, f := range intervals {
		first := f.Start
		if first.Before(until) {
			first = until
		}
		if f.End.After(first) {
			b.FlaggedSeconds += f.End.Sub(first).Seconds()
		}
		if f.End.After(until) {
			until = f.End
		}
	}
	b.Percent = 100 * b.FlaggedSeconds / b.MediaSeconds
	if b.Percent > saturationPercentLimit {
		b.Outcome = "inconclusive"
		b.Causes = append(b.Causes, "generator saturation exceeds 1% caller media wall-time")
	}
	return b
}
func callerWindow(p loadgen.ProcessReport) (time.Time, time.Time) {
	var start, end time.Time
	if observe.Numbers(p.Calls) != nil {
		return start, end
	}
	for _, c := range p.Calls {
		// This pinned public schema exposes FirstVideoSentAt, but no first audio-send
		// timestamp. Never infer media start from connection, frame counts or stats.
		if c.Call == nil || c.Call.StartedAt.IsZero() || c.Call.FirstVideoSentAt <= 0 || c.Call.SentVideo.Frames <= 0 || c.Call.HungUpAt <= c.Call.FirstVideoSentAt {
			return time.Time{}, time.Time{}
		}
		a, b := c.Call.StartedAt.Add(c.Call.FirstVideoSentAt), c.Call.StartedAt.Add(c.Call.HungUpAt)
		if start.IsZero() || a.Before(start) {
			start = a
		}
		if b.After(end) {
			end = b
		}
	}
	return start, end
}
func eventSpan(samples []loadgen.Stats, first, last time.Time) bool {
	return observe.Numbers(struct {
		Samples     []loadgen.Stats
		First, Last time.Time
	}{samples, first, last}) == nil && len(samples) > 0 && !last.Before(first) && !first.Before(samples[0].At.Add(-samples[0].Interval)) && !last.After(samples[len(samples)-1].At)
}

// Cadence is validated from stats because the public schema has no period field.
// Keep one period of edge padding; delayed pacer flags also reach backward by Max.
const eventSampleDisplacement = expectedSamplePeriod

// Unlike public soak eventWindow, a complete measurement does not extend to its
// later compaction snapshot. See docs/conductor-protocol.md's horizon audit.
func callerEventWindow(r receipt, c *callharness.Report, m callharness.MoveReport) (time.Time, time.Time, error) {
	if err := observe.Numbers(struct {
		Receipt receipt
		Call    *callharness.Report
		Move    callharness.MoveReport
	}{r, c, m}); err != nil {
		return time.Time{}, time.Time{}, err
	}
	frameEnd, fok := observe.AddCount(int64(m.Start), int64(max(m.Measurement.FirstNewFrame, m.Measurement.FirstLiveFrame)))
	audioWindow, aok := observe.AddCount(int64(m.Measurement.Audio.RecoveryLimit), int64(m.Measurement.Audio.StabilityWindow))
	videoWindow, vok := observe.AddCount(int64(m.Measurement.Video.RecoveryLimit), int64(m.Measurement.Video.StabilityWindow))
	if !fok || !aok || !vok {
		return time.Time{}, time.Time{}, fmt.Errorf("caller event window overflow")
	}
	first, last := r.Start.Add(-time.Second), r.End.Add(7*time.Second)
	// The wire format has no completeness count for matched content baselines.
	// A missing/truncated optional list cannot shorten generator coverage.
	if c.StartedAt.Before(first) {
		first = c.StartedAt
	}
	if c.HungUpAt > 0 && c.StartedAt.Add(c.HungUpAt).After(last) {
		last = c.StartedAt.Add(c.HungUpAt)
	}

	earlier := func(at time.Time) {
		if at.Before(first) {
			first = at
		}
	}
	later := func(at time.Time) {
		if at.After(last) {
			last = at
		}
	}
	earlier(c.StartedAt.Add(m.Start).Add(-time.Second))
	measurement := m.Measurement
	later(c.StartedAt.Add(max(m.End, measurement.MediaResumedAt)).Add(7 * time.Second))
	later(c.StartedAt.Add(measurement.SettledAt))
	later(c.StartedAt.Add(measurement.Audio.EvaluationEnd))
	missing := c.StartedAt.IsZero() || r.Start.IsZero() || r.End.IsZero() || m.End <= 0 || measurement.MediaResumedAt <= 0 || measurement.SettledAt <= 0 || measurement.Audio.EvaluationEnd <= 0 || measurement.Audio.RecoveryLimit <= 0 || measurement.Audio.StabilityWindow <= 0
	// Content-loss matched baselines can precede the ordinary one-second gap /
	// freshness baseline. Include their actual send spans rather than assume 1s.
	for _, baseline := range measurement.ContentLoss.Baselines {
		earlier(c.StartedAt.Add(baseline.Start))
	}
	if measurement.Audio.BaselineStart > 0 {
		earlier(c.StartedAt.Add(measurement.Audio.BaselineStart))
	}
	if measurement.VideoExpected {
		later(c.StartedAt.Add(measurement.Video.EvaluationEnd))
		if measurement.Video.BaselineStart > 0 {
			earlier(c.StartedAt.Add(measurement.Video.BaselineStart))
		}
		later(c.StartedAt.Add(time.Duration(frameEnd)))
		missing = missing || measurement.Video.EvaluationEnd <= 0 || measurement.Video.RecoveryLimit <= 0 || measurement.Video.StabilityWindow <= 0 || measurement.FirstNewFrame <= 0 || measurement.FirstLiveFrame <= 0 || measurement.FirstContent == "" || measurement.FirstNewContent == ""
	}
	// Selected caller points are insufficient when classification explored a
	// clock interval. Cover the upper bounds of every first/recovery observation,
	// the latest possible 5s freshness/stability end, and the closing gap arrival.
	if d := m.Distributed; d == nil || !d.Mapped {
		missing = true
	} else {
		if d.Start.Earliest.IsZero() || d.Start.Latest.IsZero() || d.End.Latest.IsZero() {
			missing = true
		} else {
			earlier(d.Start.Earliest.Add(-time.Second))
			later(d.End.Latest.Add(7 * time.Second))
			later(d.Start.Latest.Add(time.Duration(audioWindow)))
			if measurement.VideoExpected {
				later(d.Start.Latest.Add(time.Duration(videoWindow)))
			}
			for _, name := range []string{"resume", "first_content", "first_new_content", "first_live"} {
				if bound, ok := d.Durations[name]; ok && bound.Upper > 0 {
					later(d.Start.Latest.Add(bound.Upper))
					if name == "resume" {
						later(d.Start.Latest.Add(bound.Upper).Add(7 * time.Second))
					}
				} else if name == "resume" || measurement.VideoExpected {
					missing = true
				}
			}
			for _, gap := range d.Gaps {
				later(d.End.Latest.Add(gap.EventGap.Upper))
			}
			if d.KillToLive != nil {
				later(d.Start.Latest.Add(d.KillToLive.Upper))
			}
		}
	}
	// Loss scans until the snapshot, but fixed sends already returned by settle
	// cannot be changed by a later stall. Late returns have no exported timestamp
	// upper bound; retain the conservative observation horizon for those cases.
	missing = missing || measurement.LateAudio > 0 || measurement.LateVideoFrames > 0
	if missing {
		if c.StartedAt.IsZero() || measurement.ObservedUntil <= 0 {
			return first, time.Time{}, fmt.Errorf("caller event measurement horizon unavailable")
		}
		later(c.StartedAt.Add(measurement.ObservedUntil))
	}
	return first, last.Add(eventSampleDisplacement), nil
}
func applySaturation(dir string, report *loadgen.RunReport, receipts []receipt, processJobs []string) ([]callerSaturation, []eventExclusion) {
	jobs := map[string]bool{}
	if len(processJobs) != len(report.Processes) {
		return nil, nil
	}
	for _, job := range processJobs {
		if job == "" || jobs[job] {
			return nil, nil
		}
		jobs[job] = true
	}
	byID := map[callharness.OperationIdentity]receipt{}
	for _, r := range receipts {
		byID[r.Identity] = r
	}
	var budgets []callerSaturation
	var excluded []eventExclusion
	for pi := range report.Processes {
		p := &report.Processes[pi]
		job := processJobs[pi]
		samples, err := loadSamples(filepath.Join(dir, job), p.PID, true)
		count, countOK := observe.AddCount(int64(len(p.Samples)), int64(p.SamplesDropped))
		if err == nil && (!countOK || int64(len(samples)) != count) {
			err = fmt.Errorf("stats count differs from caller JSONL")
		}
		if err == nil {
			for i, original := range p.Samples {
				collected := samples[len(samples)-len(p.Samples)+i]
				sameTime := collected.At.Equal(original.At)
				collected.At = original.At
				if !sameTime || !reflect.DeepEqual(collected, original) {
					err = fmt.Errorf("stats values differ from caller JSONL")
					break
				}
			}
		}
		start, end := callerWindow(*p)
		if err == nil {
			err = sampleSpan(samples, start, end)
		}
		b := saturationBudget(job, samples, start, end)
		if err != nil {
			var diagnostic *generatorEvidenceError
			if errors.As(err, &diagnostic) {
				b.Checks = append(b.Checks, diagnostic.check)
			}
			b.Outcome = "inconclusive"
			b.Causes = append(b.Causes, "missing or unparsable collected stats: "+err.Error())
		}
		if _, err := os.Stat(filepath.Join(dir, job, "stop.json")); err == nil {
			b.Outcome = "inconclusive"
			b.Causes = append(b.Causes, "caller stop record present")
		}
		budgets = append(budgets, b)
		// The per-second public process flag is replaced by the budget and exclusions.
		p.Saturated = false
		for ci := range p.Calls {
			c := p.Calls[ci].Call
			if c == nil {
				continue
			}
			usable := make([]callharness.MoveReport, 0, len(c.Moves))
			for _, m := range c.Moves {
				var id callharness.OperationIdentity
				if m.Identity != nil {
					id = *m.Identity
				}
				r, ok := byID[id]
				if !ok {
					r = receipt{Start: c.StartedAt.Add(m.Start), End: c.StartedAt.Add(m.End)}
				}
				first, last, horizonErr := callerEventWindow(r, c, m)
				excursion := statsClockExcursion(samples)
				first, last = first.Add(-excursion), last.Add(excursion)
				cause := ""
				if !ok {
					cause = "caller move without server receipt"
				} else if err != nil {
					cause = "missing or unparsable collected generator stats"
				} else if horizonErr != nil {
					cause = horizonErr.Error()
				} else if !eventSpan(samples, first, last) {
					cause = "generator stats do not cover event window"
				} else {
					for _, f := range b.Flagged {
						if f.Start.Before(last) && f.End.After(first) {
							cause = "generator saturation overlaps event window"
							break
						}
					}
				}
				if cause == "" {
					usable = append(usable, m)
					continue
				}
				original := "inconclusive"
				if m.Distributed != nil {
					original = m.Distributed.Outcome
				}
				excluded = append(excluded, eventExclusion{id, job, original, "inconclusive", cause, first, last})
			}
			c.Moves = usable // Excluded trials cannot enter JudgeDistributedTrials or required counts.
		}
	}
	return budgets, excluded
}
