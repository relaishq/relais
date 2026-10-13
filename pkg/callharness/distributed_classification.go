package callharness

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"time"
)

// These are caller-local baseline observations, not server-clock estimates.
// Keep every distinct first-event baseline so later events cannot silently
// treat one possible noisy/slow first baseline as an established reference.
type FreshnessBaseline struct {
	Samples int           `json:"samples"`
	Median  time.Duration `json:"median_ns"`
	P95     time.Duration `json:"p95_ns"`
}
type FirstBaselines struct {
	Audio FreshnessBaseline `json:"audio"`
	Video FreshnessBaseline `json:"video"`
}

func baselineOf(f FreshnessReport) FreshnessBaseline {
	return FreshnessBaseline{Samples: f.BaselineSamples, Median: f.BaselineMedian, P95: f.BaselineP95}
}
func firstBaselinesOf(m EventMeasurement) FirstBaselines {
	return FirstBaselines{Audio: baselineOf(m.Audio), Video: baselineOf(m.Video)}
}
func (b FreshnessBaseline) report() FreshnessReport {
	return FreshnessReport{BaselineSamples: b.Samples, BaselineMedian: b.Median, BaselineP95: b.P95, SpreadLimit: baselineSpreadLimit}
}

// Preserve the local yardstick's first-baseline policy for each evaluation.
func normalizeEventFreshness(m *EventMeasurement, first FirstBaselines) {
	for _, p := range []struct {
		current *FreshnessReport
		first   FreshnessReport
	}{{&m.Audio, first.Audio.report()}, {&m.Video, first.Video.report()}} {
		p.current.FirstBaselineTrusted = p.first.baselineTrusted()
		p.current.FirstBaselineSamples = p.first.BaselineSamples
		p.current.FirstBaselineMedian = p.first.BaselineMedian
		p.current.FirstBaselineP95 = p.first.BaselineP95
		if p.current == &m.Audio || m.VideoExpected {
			previous := p.current.Verdict
			p.current.updateVerdict()
			if !previous.Trusted && !p.current.AbsoluteLag {
				p.current.Verdict.Trusted = false
				for _, reason := range previous.Reasons {
					if !slices.Contains(p.current.Verdict.Reasons, reason) {
						p.current.Verdict.Reasons = append(p.current.Verdict.Reasons, reason)
					}
				}
			}
		}
	}
}

func contentVerdicts(m *EventMeasurement) []*MetricVerdict {
	return []*MetricVerdict{&m.AudioLoss, &m.VideoLoss, &m.Audio.Verdict, &m.Video.Verdict, &m.ResumeVerdict, &m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict}
}

var contentMetricNames = [...]string{"audio_loss", "video_loss", "audio_freshness", "video_freshness", "resume", "first_content", "first_new_content", "first_live"}

// Only unavailable/invalid mappings invalidate all clock-dependent metrics.
// A nonzero, valid clock interval is evaluated below instead.
func invalidateDistributedContent(m *EventMeasurement, cause string) {
	for _, v := range contentVerdicts(m) {
		v.Trusted, v.Failed = false, false
		v.Reasons = append(slices.Clone(v.Reasons), cause)
	}
	m.ContentLoss.Event.Audio.Trusted = false
	m.ContentLoss.Event.Video.Trusted = false
	m.updateJudgment()
}

// eventClassificationPoints partitions the integer-nanosecond timeline at
// every branch boundary used by eventMeasurement, measureFreshness,
// classifyContent, videoRecovery and online decode coverage. Between boundaries
// membership, identities, percentiles and lateness counts are constant. The
// only varying verdict quantity (recovery delay) is affine in event start; its
// limit crossing is included too. Evaluate the boundary and both adjacent
// nanoseconds to cover strict and inclusive comparisons, not a sampling grid.
func (r *recorder) eventClassificationPoints(limit time.Duration, floors []time.Duration) (starts, ends []time.Duration) {
	policy := r.freshnessPolicy.defaults()
	starts = []time.Duration{0, r.historyStart + baselineWindow, limit - policy.EvaluationWindow()}
	ends = []time.Duration{limit - contentWindowMargin - contentSettleWindow}
	for _, floor := range floors {
		starts = append(starts, floor+baselineWindow)
	}
	unit := func(u contentUnit) {
		starts = append(starts, u.at, u.at+contentWindowMargin, u.returnedAt, u.returnedAt+baselineWindow, u.returnedAt-policy.EvaluationWindow(), u.returnedAt-policy.RecoveryLimit)
		ends = append(ends, u.at-contentWindowMargin, u.returnedAt-contentWindowMargin-contentSettleWindow, u.returnedAt)
	}
	for _, u := range r.sentAudioUnits {
		unit(u)
	}
	for _, f := range r.sentVideoFrames {
		unit(f.unit())
		starts = append(starts, f.requestedAt)
	}
	for _, u := range r.repeatedAudioReturns {
		unit(u)
	}
	for _, u := range r.repeatedVideoReturns {
		unit(u)
	}
	for _, t := range r.tracks {
		// Only margin jumps and outage-gap ends can change takeover resume.
		// Ordinary RTP packet boundaries do not affect start classifications.
		interval := opusFrameDuration
		if t.video != nil {
			interval = r.frameInterval
			if interval == 0 {
				interval = time.Second / 30
			}
		}
		for i := 1; i < len(t.arrivals); i++ {
			gap := t.arrivals[i] - t.arrivals[i-1]
			margin := i < len(t.headers) && int16(t.headers[i].seq-t.headers[i-1].seq) >= 8192-5500
			if margin || gap > max(50*time.Millisecond, 3*interval) {
				starts = append(starts, t.arrivals[i])
			}
		}
		starts = append(starts, t.unmatchedTimes...)
		for _, at := range t.unmatchedTimes {
			starts = append(starts, at+baselineWindow, at-policy.EvaluationWindow())
		}

		if t.video != nil {
			for _, f := range t.video.frames {
				starts = append(starts, f.at, f.source.at, f.source.returnedAt, f.source.requestedAt, f.source.at+contentWindowMargin)
				ends = append(ends, f.at, f.source.at, f.source.returnedAt)
				unit(f.source.unit())
			}
			for _, s := range t.video.sampleResults {
				starts = append(starts, s.at)
			}
		}
	}
	// For a move, resume is the first RTP arrival at/after end. Its
	// classification changes only when that arrival crosses a send, return,
	// decode or content-window boundary. Dense fragments between two such
	// observations need only their first/last resume values (the endpoints
	// already cover duration extrema). Retain the predecessor packet at each
	// boundary, with adjacent-nanosecond evaluations supplied by the caller.
	var packets []time.Duration
	for _, t := range r.tracks {
		packets = append(packets, t.arrivals...)
	}
	slices.Sort(packets)
	thresholds := slices.Clone(ends)
	for _, u := range r.sentAudioUnits {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	for _, f := range r.sentVideoFrames {
		thresholds = append(thresholds, f.at, f.returnedAt)
	}
	for _, u := range r.repeatedAudioReturns {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	for _, u := range r.repeatedVideoReturns {
		thresholds = append(thresholds, u.at, u.returnedAt)
	}
	if len(packets) > 0 {
		ends = append(ends, packets[len(packets)-1])
		for _, at := range thresholds {
			i := sort.Search(len(packets), func(i int) bool { return packets[i] > at }) - 1
			if i >= 0 {
				ends = append(ends, packets[i])
			}
		}
	}
	return starts, ends
}
func classificationPoints(lo, hi time.Duration, boundaries []time.Duration) []time.Duration {
	points := []time.Duration{lo, hi}
	for _, at := range boundaries {
		if at < lo || at > hi {
			continue
		}
		points = append(points, at)
		if at > lo {
			points = append(points, at-1)
		}
		if at < hi {
			points = append(points, at+1)
		}
	}
	slices.Sort(points)
	return slices.Compact(points)
}
func (r *recorder) floorClassificationPoints(floor DurationInterval) []time.Duration {
	var boundaries []time.Duration
	for _, u := range r.sentAudioUnits {
		boundaries = append(boundaries, u.returnedAt)
	}
	for _, f := range r.sentVideoFrames {
		boundaries = append(boundaries, f.returnedAt)
	}
	return classificationPoints(floor.Lower, floor.Upper, boundaries)
}

const maxClassificationEvaluations = 4096

type verdictAgreement struct {
	seen, agrees bool
	verdict      MetricVerdict
	label        string
}

func (a *verdictAgreement) add(v MetricVerdict, label string) {
	if !a.seen {
		a.seen, a.agrees = true, true
		a.verdict = MetricVerdict{Trusted: v.Trusted, Failed: v.Failed, Reasons: slices.Clone(v.Reasons)}
		a.label = label
		return
	}
	if a.verdict.Trusted != v.Trusted || a.verdict.Failed != v.Failed || a.label != label {
		a.agrees = false
	}
	for _, reason := range v.Reasons {
		if !slices.Contains(a.verdict.Reasons, reason) {
			a.verdict.Reasons = append(a.verdict.Reasons, reason)
		}
	}
}
func (a verdictAgreement) result(name string, width time.Duration) MetricVerdict {
	if !a.seen || !a.agrees {
		reasons := slices.Clone(a.verdict.Reasons)
		reasons = append(reasons, fmt.Sprintf("%s classification changes within event-time interval; width=%s", name, width))
		return MetricVerdict{Reasons: reasons}
	}
	v := a.verdict
	if !v.Trusted {
		v.Reasons = append(v.Reasons, fmt.Sprintf("%s not proven throughout event-time interval; width=%s", name, width))
	}
	return v
}

func recordDuration(ranges map[string]DurationInterval, name string, value time.Duration, present bool) {
	if !present {
		return
	}
	old, ok := ranges[name]
	if !ok {
		ranges[name] = DurationInterval{Lower: value, Upper: value}
		return
	}
	old.Lower, old.Upper = min(old.Lower, value), max(old.Upper, value)
	ranges[name] = old
}
func measurementDurations(ranges map[string]DurationInterval, p MoveReport, m EventMeasurement, r *recorder, limit time.Duration) {
	recordDuration(ranges, "resume", m.MediaResumedAt-p.Start, m.MediaResumedAt > 0)
	recordDuration(ranges, "resume_observed", m.MediaResumedAt, m.MediaResumedAt > 0)
	recordDuration(ranges, "audio_freshness", m.Audio.BackToBaseline, m.Audio.Recovered)
	recordDuration(ranges, "video_freshness", m.Video.BackToBaseline, m.VideoExpected && m.Video.Recovered)
	recordDuration(ranges, "first_new_content", m.FirstNewFrame, m.VideoExpected && m.FirstNewContent != "")
	recordDuration(ranges, "first_live", m.FirstLiveFrame, m.VideoExpected && m.FirstLiveFrame > 0)
	first := time.Duration(0)
	if m.VideoExpected && m.MediaResumedAt > 0 {
		for _, t := range r.tracks {
			if t.video != nil {
				for _, f := range t.video.frames {
					if f.decodable && f.at >= m.MediaResumedAt && f.at < limit && (first == 0 || f.at < first) {
						first = f.at
					}
				}
			}
		}
	}
	recordDuration(ranges, "first_content", first-p.Start, first > 0)
}
func nextBaselineFloor(p MoveReport, m EventMeasurement, current time.Duration) time.Duration {
	if p.Kind != "takeover" {
		return current
	}
	if m.Audio.Recovered && (!m.VideoExpected || m.Video.Recovered) {
		return p.Start + max(m.Audio.BackToBaseline, m.Video.BackToBaseline)
	}
	return max(p.End, m.MediaResumedAt)
}

func (r *recorder) plannedMoveSpan(receipt *DistributedEvent) DurationInterval {
	rate := r.distributed.Clock.DriftPPM() / 1e6
	raw := receipt.End.Sub(receipt.Start)
	hostWidth := r.distributed.ControlOffset.Upper - r.distributed.ControlOffset.Lower
	return DurationInterval{Lower: max(0, time.Duration(math.Floor(float64(raw)/(1+rate)))-hostWidth), Upper: time.Duration(math.Ceil(float64(raw)/(1-rate))) + hostWidth}
}

func (r *recorder) selectDistributedPoint(report *MoveReport) {
	d := report.Distributed
	report.Start, report.End = r.since(d.Start.Earliest), r.since(d.End.Latest)
	if report.Kind != "move" {
		return
	}
	span := r.plannedMoveSpan(report.Receipt)
	lower := max(report.Start, r.since(d.End.Earliest)-span.Upper)
	upper := min(r.since(d.Start.Latest), report.End-span.Lower)
	if lower > upper {
		d.Mapped = false
		d.WindowOutcome = "inconclusive"
		d.Outcome = "inconclusive"
		d.Causes = append(d.Causes, "move timestamps inconsistent with shared control-clock rate bound")
		report.Start, report.End = 0, 0
		return
	}
	d.Start.Earliest, d.Start.Latest = r.start.Add(lower), r.start.Add(upper)
	report.Start = lower
	report.End = max(r.since(d.End.Earliest), lower+span.Lower)
	report.End = min(report.End, min(r.since(d.End.Latest), lower+span.Upper))
}

// classifyDistributedEvent changes verdicts only. Raw caller gaps, content
// ages, counts, classifications and the selected point measurement are retained.
// Takeover start/end are independent host intervals. Planned-move timestamps
// share a control clock: keep their measured span, widened only for relative
// rate error, so WAN offset uncertainty cannot make a short move reverse time.
func (r *recorder) classifyDistributedEvent(report *MoveReport, limit time.Duration, floor DurationInterval, first []FirstBaselines, self bool) {
	d := report.Distributed
	if !d.Mapped {
		invalidateDistributedContent(&report.Measurement, "event clock mapping unavailable")
		d.BaselineFloor = DurationInterval{Lower: floor.Lower, Upper: max(floor.Upper, limit)}
		return
	}
	lo, hi := r.since(d.Start.Earliest), r.since(d.Start.Latest)
	endLo, endHi := r.since(d.End.Earliest), r.since(d.End.Latest)
	width := hi - lo
	d.Durations = map[string]DurationInterval{}
	d.FirstBaselines = slices.Clone(first)
	d.BaselineFloor = DurationInterval{Lower: time.Duration(math.MaxInt64), Upper: time.Duration(math.MinInt64)}
	var agreements [len(contentMetricNames)]verdictAgreement
	floors := r.floorClassificationPoints(floor)
	startBoundaries, endBoundaries := r.eventClassificationPoints(limit, floors)
	var span DurationInterval
	if report.Kind == "move" {
		span = r.plannedMoveSpan(report.Receipt)
		for _, at := range endBoundaries {
			startBoundaries = append(startBoundaries, at-span.Lower, at-span.Upper)
		}
		startBoundaries = append(startBoundaries, endLo-span.Lower, endLo-span.Upper, endHi-span.Lower, endHi-span.Upper)
	}
	starts := classificationPoints(lo, hi, startBoundaries)
	complete := true
	evaluate := func(start, end, baselineFloor time.Duration) {
		p := *report
		p.Start, p.End = start, end
		if p.Kind == "takeover" {
			p.Recovery = r.classificationRecovery(start, limit)
		}
		m := r.classificationMeasurement(p, limit, baselineFloor)
		refs := first
		if self {
			ref := firstBaselinesOf(m)
			refs = []FirstBaselines{ref}
			if !slices.Contains(d.FirstBaselines, ref) {
				d.FirstBaselines = append(d.FirstBaselines, ref)
			}
		}
		if len(refs) == 0 {
			refs = []FirstBaselines{firstBaselinesOf(m)}
		}
		for _, ref := range refs {
			if d.Evaluations >= maxClassificationEvaluations {
				complete = false
				return
			}
			d.Evaluations++
			candidate := m
			normalizeEventFreshness(&candidate, ref)
			if !self && len(first) == 0 {
				candidate.Audio.Verdict.untrust("first event baseline interval unavailable")
				if candidate.VideoExpected {
					candidate.Video.Verdict.untrust("first event baseline interval unavailable")
				}
			}
			for i, v := range contentVerdicts(&candidate) {
				label := ""
				if v.Trusted && !v.Failed {
					switch i {
					case 5:
						label = candidate.FirstContent
					case 6:
						label = candidate.FirstNewContent
					}
				}
				agreements[i].add(*v, label)
			}
			measurementDurations(d.Durations, p, candidate, r, limit)
			next := nextBaselineFloor(p, candidate, baselineFloor)
			d.BaselineFloor.Lower, d.BaselineFloor.Upper = min(d.BaselineFloor.Lower, next), max(d.BaselineFloor.Upper, next)
		}
	}
	// The zero-uncertainty path uses the already normalized local measurement
	// verbatim, including reasons and baseline policy; it cannot drift from local.
	if lo == hi && endLo == endHi && floor.Lower == floor.Upper && (self || len(first) <= 1) {
		m := report.Measurement
		for i, v := range contentVerdicts(&m) {
			agreements[i].add(*v, "")
		}
		if self {
			d.FirstBaselines = []FirstBaselines{firstBaselinesOf(m)}
		}
		measurementDurations(d.Durations, *report, m, r, limit)
		next := nextBaselineFloor(*report, m, floor.Lower)
		d.BaselineFloor = DurationInterval{Lower: next, Upper: next}
		d.Evaluations = 1
	} else {
		for _, start := range starts {
			lower, upper := endLo, endHi
			if report.Kind == "move" {
				lower, upper = max(lower, start+span.Lower), min(upper, start+span.Upper)
			}
			if lower > upper {
				continue
			}
			ends := classificationPoints(lower, upper, endBoundaries)
			for _, end := range ends {
				for _, f := range floors {
					evaluate(start, end, f)
					if !complete {
						break
					}
				}
				if !complete {
					break
				}
			}
			if !complete {
				break
			}
		}
	}
	// Resume can move between dense RTP fragments without changing any
	// availability/content classification. Bound its observed instant over
	// the entire end range, then subtract the whole origin interval; interior
	// packet sawtooths cannot make the reported elapsed bound too narrow.
	if observed, ok := d.Durations["resume_observed"]; ok {
		d.Durations["resume"] = DurationInterval{Lower: max(0, observed.Lower-hi), Upper: observed.Upper - lo}
		delete(d.Durations, "resume_observed")
	}
	for i, v := range contentVerdicts(&report.Measurement) {
		if !complete || d.Evaluations == 0 {
			*v = MetricVerdict{Reasons: []string{fmt.Sprintf("interval evaluation unavailable or exceeds %d evaluations; width=%s", maxClassificationEvaluations, width)}}
		} else {
			*v = agreements[i].result(contentMetricNames[i], max(width, endHi-endLo))
		}
		if duration, ok := d.Durations[contentMetricNames[i]]; ok && width > 0 && !v.Trusted {
			v.Reasons = append(v.Reasons, fmt.Sprintf("duration=[%s,%s]; event interval width=%s", duration.Lower, duration.Upper, width))
		}
	}
	if !complete || d.Evaluations == 0 {
		d.FirstBaselines = nil
		d.BaselineFloor = DurationInterval{Lower: floor.Lower, Upper: max(floor.Upper, limit)}
	}
	// The union/intersection loss proof and identified-live proof are independent
	// of the classification cells. Neither can be replaced by an RTP point result.
	mergeIntervalVerdict(&report.Measurement.AudioLoss, d.AudioLoss)
	mergeIntervalVerdict(&report.Measurement.VideoLoss, d.VideoLoss)
	if report.Kind == "takeover" && d.KillToLive == nil {
		report.Measurement.FirstLiveVerdict.untrust("first identified post-recovery live content unavailable")
	}
	r.failIdentityMetrics(&report.Measurement)
	report.Measurement.ContentLoss.Event.Audio.Trusted = report.Measurement.ContentLoss.Event.Audio.Trusted && report.Measurement.AudioLoss.Trusted
	report.Measurement.ContentLoss.Event.Video.Trusted = report.Measurement.ContentLoss.Event.Video.Trusted && report.Measurement.VideoLoss.Trusted
	report.Measurement.updateJudgment()
	d.AudioLoss, d.VideoLoss = report.Measurement.AudioLoss, report.Measurement.VideoLoss
	if d.WindowOutcome == "" {
		d.WindowOutcome = "pass"
	}
	d.Causes = append(d.Causes, report.Measurement.Reasons...)
	if report.Measurement.Inconclusive {
		d.WindowOutcome = "inconclusive"
	}
	if report.Measurement.Failed || report.Lost {
		d.WindowOutcome = "fail"
	}
	if report.Lost {
		d.Causes = append(d.Causes, "server reported call loss: "+report.LossCause)
	}
	if (report.Error != "" || report.Receipt.Uncertain) && d.WindowOutcome != "fail" {
		d.WindowOutcome = "inconclusive"
		d.Causes = append(d.Causes, "server operation failed or outcome uncertain")
	}
	d.Outcome = d.WindowOutcome
	for _, g := range d.Gaps {
		if g.Outcome == "fail" {
			d.Outcome = "fail"
		}
		if g.Outcome == "inconclusive" && d.Outcome != "fail" {
			d.Outcome = "inconclusive"
		}
		d.Causes = append(d.Causes, g.Causes...)
	}
	if report.Kind == "takeover" && report.Measurement.VideoExpected {
		if live, ok := d.Durations["first_live"]; ok {
			d.KillToLive = &live
		} else {
			d.KillToLive = nil
		}
	}
}

// Both independent interval proofs must establish trust. Identity corruption is
// reapplied separately because it proves failure independently of clock trust.
func mergeIntervalVerdict(dst *MetricVerdict, proof MetricVerdict) {
	dst.Trusted = dst.Trusted && proof.Trusted
	dst.Failed = dst.Trusted && (dst.Failed || proof.Failed)
	for _, reason := range proof.Reasons {
		if !slices.Contains(dst.Reasons, reason) {
			dst.Reasons = append(dst.Reasons, reason)
		}
	}
}
