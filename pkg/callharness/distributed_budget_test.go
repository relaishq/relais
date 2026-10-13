package callharness

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Frozen pre-120 classification: budget counts each baseline reference.
// Unlimited runs are the oracle for verdicts, reasons and duration bounds.
func (r *recorder) referenceCountClassify(report *MoveReport, limit time.Duration, floor DurationInterval, first []FirstBaselines, self bool, budget int) {
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
	floors := r.floorClassificationPoints(floor)
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
			if d.Evaluations >= budget {
				complete = false
				return
			}
			d.Evaluations++
			if d.Evaluations%32 == 0 {
				runtime.Gosched()
			}
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

func budgetPoint(r *recorder, index int, limit, floor time.Duration) MoveReport {
	move := r.moves[index]
	rep := MoveReport{Kind: move.kind, Receipt: move.event, Distributed: r.distributedMeasurement(move, limit)}
	if rep.Distributed.Mapped {
		r.selectDistributedPoint(&rep)
	}
	if rep.Kind == "takeover" {
		rep.Recovery = r.videoRecovery(rep.Start, limit)
	}
	rep.Measurement = r.eventMeasurement(rep, limit, floor)
	return rep
}

// Independent grid cardinality, using the unfiltered pre-118 boundaries.
func budgetCells(r *recorder, p MoveReport, limit time.Duration, floor DurationInterval) int {
	d := p.Distributed
	lo, hi := r.since(d.Start.Earliest), r.since(d.Start.Latest)
	endLo, endHi := r.since(d.End.Earliest), r.since(d.End.Latest)
	floors := r.floorClassificationPoints(floor)
	sb, eb := r.legacyEventClassificationPoints(limit, floors)
	var span DurationInterval
	if p.Kind == "move" {
		span = r.plannedMoveSpan(p.Receipt)
		for _, at := range eb {
			sb = append(sb, at-span.Lower, at-span.Upper)
		}
		sb = append(sb, endLo-span.Lower, endLo-span.Upper, endHi-span.Lower, endHi-span.Upper)
	}
	cells := 0
	for _, start := range classificationPoints(lo, hi, sb) {
		lower, upper := endLo, endHi
		if p.Kind == "move" {
			lower = max(lower, start+span.Lower)
			upper = min(upper, start+span.Upper)
		}
		if lower <= upper {
			cells += len(classificationPoints(lower, upper, eb)) * len(floors)
		}
	}
	return cells
}

func jitterBudgetFixture(r *recorder) {
	for key, u := range r.sentAudioUnits {
		u.returnedAt += time.Duration((int(u.at/time.Millisecond)*37)%997) * time.Microsecond
		r.sentAudioUnits[key] = u
	}
	for key, f := range r.sentVideoFrames {
		f.returnedAt += time.Duration((int(f.at/time.Millisecond)*53)%991) * time.Microsecond
		r.sentVideoFrames[key] = f
	}
	for _, track := range r.tracks {
		if track.video != nil {
			for i := range track.video.frames {
				f := &track.video.frames[i]
				source := r.sentVideoFrames[f.pictureID]
				delta := source.returnedAt - f.source.returnedAt
				f.source = source
				f.at += delta
				f.receivedAt += delta
				f.firstArrival += delta
			}
		}
	}
}

func TestDistributedCellBudgetEquivalence(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		for _, kind := range []string{"move", "takeover"} {
			t.Run(fmt.Sprintf("%s/jitter=%t", kind, jitter), func(t *testing.T) {
				r, c := denseDistributedCall(t, true, 4*time.Millisecond, 3*time.Millisecond)
				if jitter {
					jitterBudgetFixture(r)
				}
				start, end := 5*time.Second, 5030*time.Millisecond
				if kind == "takeover" {
					start, end = 10*time.Second, 10500*time.Millisecond
				}
				recordDenseEvent(t, r, c, "event", kind, start, end)
				r.prepareMeasurementIndex()
				floor := DurationInterval{Lower: start - 1100*time.Millisecond, Upper: start - 950*time.Millisecond}
				limit := start + 7*time.Second
				makePoint := func() MoveReport { return budgetPoint(r, 0, limit, floor.Lower) }
				initial := makePoint()
				ref := firstBaselinesOf(initial.Measurement)
				refs := []FirstBaselines{ref, ref, ref, ref}
				refs[1].Audio.Samples++
				refs[2].Video.Samples++
				// Every reference must contribute, including a known absolute failure.
				refs[3].Audio.Median = 70 * time.Millisecond
				refs[3].Audio.P95 = 72 * time.Millisecond
				cells := budgetCells(r, initial, limit, floor)
				require.Positive(t, cells)
				before, after := makePoint(), makePoint()
				r.referenceCountClassify(&before, limit, floor, refs, false, 1<<30)
				r.classifyDistributedEventWithBudget(&after, limit, floor, refs, false, 1<<30)
				require.Equal(t, cells*len(refs), before.Distributed.Evaluations)
				require.Equal(t, cells, after.Distributed.Evaluations)
				require.Equal(t, contentVerdicts(&before.Measurement), contentVerdicts(&after.Measurement))
				require.Equal(t, before.Distributed.Durations, after.Distributed.Durations)
				require.Equal(t, before.Distributed.BaselineFloor, after.Distributed.BaselineFloor)
				production := makePoint()
				r.classifyDistributedEvent(&production, limit, floor, refs, false)
				if cells <= maxClassificationEvaluations {
					require.Equal(t, cells, production.Distributed.Evaluations)
					require.Equal(t, contentVerdicts(&after.Measurement), contentVerdicts(&production.Measurement))
					require.Equal(t, after.Distributed.Durations, production.Distributed.Durations)
				} else {
					require.Equal(t, maxClassificationEvaluations, production.Distributed.Evaluations)
				}
				exact := makePoint()
				r.classifyDistributedEventWithBudget(&exact, limit, floor, refs, false, cells)
				require.Equal(t, contentVerdicts(&after.Measurement), contentVerdicts(&exact.Measurement))
				require.Equal(t, after.Distributed.Durations, exact.Distributed.Durations)
				exhausted := makePoint()
				r.classifyDistributedEventWithBudget(&exhausted, limit, floor, refs, false, cells-1)
				require.Equal(t, cells-1, exhausted.Distributed.Evaluations)
				require.Nil(t, exhausted.Distributed.FirstBaselines)
			})
		}
	}
}

func TestDistributedExhaustedEventKeepsFirstBaselines(t *testing.T) {
	r, c := denseDistributedCall(t, true, 0, 0)
	// Known first-event latency failure. A later uncertain floor must not erase
	// that independent failure, nor turn it into a new local baseline.
	for key, u := range r.sentAudioUnits {
		u.returnedAt += 100 * time.Millisecond
		r.sentAudioUnits[key] = u
	}
	for key, f := range r.sentVideoFrames {
		f.returnedAt += 100 * time.Millisecond
		r.sentVideoFrames[key] = f
	}
	for _, track := range r.tracks {
		for i := range track.arrivals {
			track.arrivals[i] += 100 * time.Millisecond
		}
		if track.video != nil {
			for i := range track.video.frames {
				f := &track.video.frames[i]
				f.at += 100 * time.Millisecond
				f.receivedAt += 100 * time.Millisecond
				f.source.returnedAt += 100 * time.Millisecond
			}
		}
	}
	r.distributed.WorkerOffset = OffsetInterval{Lower: -3100 * time.Millisecond, Upper: 3100 * time.Millisecond}
	recordDenseEvent(t, r, c, "first", "move", 5*time.Second, 5030*time.Millisecond)
	recordDenseEvent(t, r, c, "exhausted", "takeover", 10*time.Second, 14*time.Second)
	r.moves[1].armedAt = r.start.Add(6 * time.Second)
	recordDenseEvent(t, r, c, "third", "move", 15*time.Second, 15030*time.Millisecond)
	reports := r.measureMoves(r.hungUpAt, 3)
	require.NotEmpty(t, reports[0].Distributed.FirstBaselines)
	require.Equal(t, 1, reports[0].Distributed.Evaluations)
	require.Equal(t, maxClassificationEvaluations, reports[1].Distributed.Evaluations)
	require.Nil(t, reports[1].Distributed.FirstBaselines)
	require.Equal(t, reports[0].Distributed.FirstBaselines, reports[2].Distributed.FirstBaselines)
	require.Less(t, reports[2].Distributed.Evaluations, maxClassificationEvaluations)
	// Compare an uncapped third-event evaluation using the same conservative
	// floor from the exhausted event. Exhaustion must still widen that floor;
	// only the independently established first baseline survives the handoff.
	r.prepareMeasurementIndex()
	floor := reports[1].Distributed.BaselineFloor
	uncapped := budgetPoint(r, 2, r.hungUpAt, floor.Lower)
	r.referenceCountClassify(&uncapped, r.hungUpAt, floor, reports[0].Distributed.FirstBaselines, false, 1<<30)
	require.Equal(t, uncapped.Measurement.Audio.Verdict, reports[2].Measurement.Audio.Verdict)
	require.Equal(t, uncapped.Measurement.Video.Verdict, reports[2].Measurement.Video.Verdict)
	require.True(t, reports[2].Measurement.Audio.Verdict.Trusted)
	require.True(t, reports[2].Measurement.Audio.Verdict.Failed)
}

func TestDistributedExhaustedFirstBaselineStaysUnknown(t *testing.T) {
	r, c := denseDistributedCall(t, true, 0, 0)
	r.distributed.WorkerOffset = OffsetInterval{Lower: -3100 * time.Millisecond, Upper: 3100 * time.Millisecond}
	recordDenseEvent(t, r, c, "first", "takeover", 10*time.Second, 14*time.Second)
	r.moves[0].armedAt = r.start.Add(6 * time.Second)
	recordDenseEvent(t, r, c, "later", "move", 15*time.Second, 15030*time.Millisecond)
	reports := r.measureMoves(r.hungUpAt, 2)
	require.Equal(t, maxClassificationEvaluations, reports[0].Distributed.Evaluations)
	require.Nil(t, reports[0].Distributed.FirstBaselines)
	require.Nil(t, reports[1].Distributed.FirstBaselines)
	require.Less(t, reports[1].Distributed.Evaluations, maxClassificationEvaluations)
	require.False(t, reports[1].Measurement.Audio.Verdict.Trusted)
	require.Contains(t, reports[1].Measurement.Audio.Verdict.Reasons, "first event baseline interval unavailable")
}

func TestDistributedZeroIntervalCountsOneCellWithReferences(t *testing.T) {
	r, c := denseDistributedCall(t, true, 0, 0)
	recordDenseEvent(t, r, c, "move", "move", 5*time.Second, 5030*time.Millisecond)
	r.prepareMeasurementIndex()
	makePoint := func() MoveReport { return budgetPoint(r, 0, r.hungUpAt, 0) }
	point := makePoint()
	ref := firstBaselinesOf(point.Measurement)
	refs := []FirstBaselines{ref, ref, ref, ref}
	refs[3].Audio.Median = 70 * time.Millisecond
	refs[3].Audio.P95 = 72 * time.Millisecond
	before, after := makePoint(), makePoint()
	r.referenceCountClassify(&before, r.hungUpAt, DurationInterval{}, refs, false, 1<<30)
	r.classifyDistributedEventWithBudget(&after, r.hungUpAt, DurationInterval{}, refs, false, 1)
	require.Equal(t, 4, before.Distributed.Evaluations)
	require.Equal(t, 1, after.Distributed.Evaluations)
	require.Equal(t, contentVerdicts(&before.Measurement), contentVerdicts(&after.Measurement))
	require.Equal(t, before.Distributed.Durations, after.Distributed.Durations)
}
