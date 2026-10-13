package callharness

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"time"
)

// Frozen main 40b146f classifier and three-point grid; independent of #123 reductions.
func pre123Points(lo, hi time.Duration, boundaries []time.Duration) []time.Duration {
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
func (r *recorder) pre123FloorPoints(floor DurationInterval) []time.Duration {
	var boundaries []time.Duration
	for _, u := range r.sentAudioUnits {
		boundaries = append(boundaries, u.returnedAt)
	}
	for _, f := range r.sentVideoFrames {
		boundaries = append(boundaries, f.returnedAt)
	}
	return pre123Points(floor.Lower, floor.Upper, boundaries)
}

func (r *recorder) pre123Classify(report *MoveReport, limit time.Duration, floor DurationInterval, first []FirstBaselines, self bool, budget int) {
	d := report.Distributed
	if r.prepared != nil {
		clear(r.prepared.audioWork.cache)
		clear(r.prepared.videoWork.cache)
	}
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
	floors := r.pre123FloorPoints(floor)
	var span DurationInterval
	if report.Kind == "move" {
		span = r.plannedMoveSpan(report.Receipt)
	}
	startBounds := DurationInterval{Lower: lo, Upper: hi}
	endBounds := DurationInterval{Lower: endLo, Upper: endHi}
	if report.Kind == "move" {
		endBounds.Lower = min(endLo, lo+span.Lower)
		endBounds.Upper = max(endHi, hi+span.Upper)
	}
	startBoundaries, endBoundaries := r.eventClassificationPoints(limit, floors, startBounds, endBounds)
	if report.Kind == "move" {
		for _, at := range endBoundaries {
			startBoundaries = append(startBoundaries, at-span.Lower, at-span.Upper)
		}
		startBoundaries = append(startBoundaries, endLo-span.Lower, endLo-span.Upper, endHi-span.Lower, endHi-span.Upper)
	}
	starts := pre123Points(lo, hi, startBoundaries)
	complete := true
	evaluate := func(start, end, baselineFloor time.Duration) {
		// One cell is evaluated against every first-baseline reference. References
		// do not consume extra budget and must never be cut off within a cell.
		if d.Evaluations >= budget {
			complete = false
			return
		}
		d.Evaluations++
		if d.Evaluations%32 == 0 {
			runtime.Gosched()
		}
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
			ends := pre123Points(lower, upper, endBoundaries)
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
			*v = MetricVerdict{Reasons: []string{fmt.Sprintf("interval evaluation unavailable or exceeds %d evaluations; width=%s", budget, width)}}
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
