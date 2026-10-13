package callharness

import (
	"cmp"
	"fmt"
	"github.com/stretchr/testify/require"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Pre-118 evaluation pipeline with #120's cell budget and baseline handoff.
// Keep it independent of the optimized freshness and boundary paths; compare
// every field of MoveReport evidence. The frozen pre-120 budget oracle lives
// in distributed_budget_test.go.
func (r *recorder) legacyMeasureMoves(until time.Duration, count int) []MoveReport {
	// Receipts may arrive out of event order. Keep their storage order (also
	// used by maintenance snapshots), but find window limits on the mapped
	// event-time axis. An unmapped receipt provides no safe truncation point.
	var mappedStarts []time.Duration
	var startsByEvent map[*DistributedEvent]time.Duration
	if r.distributed != nil {
		r.prepareMeasurementIndex()
		defer func() { r.prepared = nil }()
		startsByEvent = make(map[*DistributedEvent]time.Duration, len(r.moves))
		for _, move := range r.moves {
			start, end, err := r.mappedMoveTimes(move)
			if err != nil {
				continue
			}
			startTime := TimeInterval{Earliest: r.start.Add(start.Lower), Latest: r.start.Add(start.Upper)}
			endTime := TimeInterval{Earliest: r.start.Add(end.Lower), Latest: r.start.Add(end.Upper)}
			if !distributedEventOrderingValid(move, startTime, endTime) {
				continue
			}
			mappedStarts = append(mappedStarts, start.Lower)
			startsByEvent[move.event] = start.Lower
		}
		slices.Sort(mappedStarts)
	}
	reports := make([]MoveReport, 0, count)
	baselineFloor := r.baselineFloor
	firstAudio, firstVideo := r.firstAudio, r.firstVideo
	distributedFloor := DurationInterval{Lower: baselineFloor, Upper: baselineFloor}
	var firstDistributed []FirstBaselines
	if len(r.savedMoves) > 0 {
		if d := r.savedMoves[0].Distributed; d != nil {
			firstDistributed = d.FirstBaselines
		}
		if d := r.savedMoves[len(r.savedMoves)-1].Distributed; d != nil {
			distributedFloor = d.BaselineFloor
		}
	}
	for moveIndex, move := range r.moves[:count] {
		report := MoveReport{
			Kind:          move.kind,
			Lost:          move.lost,
			LossCause:     move.lossCause,
			DetectionTime: move.detection,
			From:          move.from,
			To:            move.to,
			Start:         r.since(move.start),
			End:           r.since(move.end),
			Result:        move.result,
		}
		if move.event != nil {
			report.Identity = &move.event.Identity
			report.Receipt = move.event
			report.Distributed = r.distributedMeasurement(move, until)
			if report.Distributed.Mapped {
				r.selectDistributedPoint(&report)
			} else {
				report.Start, report.End = 0, 0
			}
		}
		if report.Kind == "" {
			report.Kind = "move"
		}
		if move.err != nil {
			report.Error = move.err.Error()
		}
		for _, track := range r.tracks {
			around := track.aroundMove(report.Start, report.End)
			var gaps []time.Duration
			floor := max(report.Start-time.Second, baselineFloor)
			for i := 1; i < len(track.arrivals); i++ {
				if track.arrivals[i-1] >= floor && track.arrivals[i] < report.Start {
					gaps = append(gaps, track.arrivals[i]-track.arrivals[i-1])
				}
			}
			slices.Sort(gaps)
			around.BaselineGapSamples = len(gaps)
			if len(gaps) > 0 {
				around.BaselineGapP95 = gaps[(95*len(gaps)+99)/100-1]
			}
			report.Tracks = append(report.Tracks, around)
		}
		limit := until
		if move.event != nil {
			if start, ok := startsByEvent[move.event]; ok {
				next := sort.Search(len(mappedStarts), func(i int) bool { return mappedStarts[i] > start })
				if next < len(mappedStarts) {
					limit = min(limit, mappedStarts[next])
				}
			}
		} else if moveIndex+1 < len(r.moves) {
			limit = min(limit, r.since(r.moves[moveIndex+1].start))
		}
		if report.Kind == "takeover" {
			report.Recovery = r.videoRecovery(report.Start, limit)
		}
		report.Measurement = r.legacyMeasureEvent(report, limit, baselineFloor, false)
		if len(r.savedMoves) == 0 && len(reports) == 0 {
			firstAudio, firstVideo = report.Measurement.Audio, report.Measurement.Video
		}
		for _, pair := range []struct {
			current *FreshnessReport
			first   FreshnessReport
		}{{&report.Measurement.Audio, firstAudio}, {&report.Measurement.Video, firstVideo}} {
			pair.current.FirstBaselineTrusted = pair.first.baselineTrusted()
			pair.current.FirstBaselineSamples = pair.first.BaselineSamples
			pair.current.FirstBaselineMedian = pair.first.BaselineMedian
			pair.current.FirstBaselineP95 = pair.first.BaselineP95
			if pair.current == &report.Measurement.Audio || report.Measurement.VideoExpected {
				previous := pair.current.Verdict
				pair.current.updateVerdict()
				if !previous.Trusted && !pair.current.AbsoluteLag {
					pair.current.Verdict.Trusted = false
					for _, reason := range previous.Reasons {
						if !slices.Contains(pair.current.Verdict.Reasons, reason) {
							pair.current.Verdict.Reasons = append(pair.current.Verdict.Reasons, reason)
						}
					}
				}
			}
		}
		if report.Distributed != nil {
			self := len(r.savedMoves) == 0 && len(reports) == 0
			r.legacyClassifyDistributedEvent(&report, limit, distributedFloor, firstDistributed, self)
			if self {
				firstDistributed = report.Distributed.FirstBaselines
			}
			distributedFloor = report.Distributed.BaselineFloor
		}
		r.failIdentityMetrics(&report.Measurement)
		report.Measurement.updateJudgment()
		if report.Kind == "takeover" {
			previous := report.Measurement
			if previous.Audio.Recovered && (r.sentVideo.Frames == 0 || previous.Video.Recovered) {
				baselineFloor = report.Start + max(previous.Audio.BackToBaseline, previous.Video.BackToBaseline)
			} else {
				// Keep the next window finite. Its own baseline is judged against
				// the call's first baseline, so lasting lag cannot become normal.
				baselineFloor = max(report.End, previous.MediaResumedAt)
			}
		}
		firstResumed := time.Duration(0)
		for _, track := range r.tracks {
			i := sort.Search(len(track.arrivals), func(i int) bool { return track.arrivals[i] > report.End })
			if i < len(track.arrivals) && (firstResumed == 0 || track.arrivals[i] < firstResumed) {
				firstResumed = track.arrivals[i]
			}
		}
		report.resumedAfterEnd = firstResumed
		if firstResumed != 0 {
			for _, at := range r.decryptFailureTimes {
				if at >= firstResumed {
					report.DecryptionFailuresAfterResume++
				}
			}
		}
		reports = append(reports, report)
	}

	return reports
}
func (r *recorder) legacyClassifyDistributedEvent(report *MoveReport, limit time.Duration, floor DurationInterval, first []FirstBaselines, self bool) {
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
	floors := r.classificationFloors(floor, lo, report.Kind)
	startBoundaries, endBoundaries := r.legacyEventClassificationPoints(limit, floors)
	var span DurationInterval
	if report.Kind == "move" {
		span = r.plannedMoveSpan(report.Receipt)
		for _, at := range endBoundaries {
			startBoundaries = append(startBoundaries, at-span.Lower, at-span.Upper)
		}
		startBoundaries = append(startBoundaries, endLo-span.Lower, endLo-span.Upper, endHi-span.Lower, endHi-span.Upper)
	}
	starts := classificationStartPoints(lo, hi, startBoundaries)
	complete := true
	evaluate := func(start, end, baselineFloor time.Duration) {
		if d.Evaluations >= maxClassificationEvaluations {
			complete = false
			return
		}
		d.Evaluations++
		p := *report
		p.Start, p.End = start, end
		if p.Kind == "takeover" {
			p.Recovery = r.classificationRecovery(start, limit)
		}
		m := r.legacyMeasureEvent(p, limit, baselineFloor, true)
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
func (r *recorder) legacyMeasureEvent(move MoveReport, limit, baselineFloor time.Duration, classificationOnly bool) EventMeasurement {
	m := EventMeasurement{WindowStart: max(0, move.Start-contentWindowMargin), SettleWindow: contentSettleWindow, MediaResumedAt: move.Recovery.MediaResumedAt, ObservedUntil: r.hungUpAt, VideoExpected: r.sentVideo.Frames > 0 || r.sentVideo.SSRC != 0}
	for _, track := range move.Tracks {
		switch track.Kind {
		case kindAudio:
			m.AudioPause = track.Gap
		case kindVideo:
			m.VideoPause = track.Gap
		}
	}
	if move.Kind != "takeover" {
		for _, track := range r.tracks {
			first := sort.Search(len(track.arrivals), func(i int) bool { return track.arrivals[i] >= move.End })
			for _, at := range track.arrivals[first:] {
				if at >= move.End && at < limit {
					if m.MediaResumedAt == 0 || at < m.MediaResumedAt {
						m.MediaResumedAt = at
					}
					break
				}
			}
		}
	}
	m.WindowEnd = max(move.End, m.MediaResumedAt) + contentWindowMargin
	m.SettledAt = m.WindowEnd + m.SettleWindow
	m.AudioLoss, m.VideoLoss = MetricVerdict{Trusted: true}, MetricVerdict{Trusted: true}
	if r.hungUpAt < m.SettledAt {
		m.AudioLoss.untrust("content settle window truncated by hangup")
		m.VideoLoss.untrust("content settle window truncated by hangup")
	}
	var audio, video []contentUnit
	if r.prepared != nil {
		audio, video = r.prepared.audio.sent, r.prepared.video.sent
	} else {
		for identity, u := range r.sentAudioUnits {
			if u.written {
				u.identity = identity
				audio = append(audio, u)
			}
		}
		for identity, f := range r.sentVideoFrames {
			if f.written {
				u := f.unit()
				u.identity = strconv.FormatUint(f.identity, 10) + ":" + strconv.FormatUint(uint64(identity), 10)
				video = append(video, u)
			}
		}
	}
	if len(audio) == 0 {
		m.AudioLoss.untrust("no identified sent audio")
	}
	if m.VideoExpected && len(video) == 0 {
		m.VideoLoss.untrust("no identified sent video")
	}
	lossAudio, lossVideo := audio, video
	if classificationOnly && r.prepared != nil {
		lossAudio = r.indexedUnits(false, false, m.WindowStart, m.WindowEnd)
		lossVideo = r.indexedUnits(true, false, m.WindowStart, m.WindowEnd)
	}
	for _, u := range lossAudio {
		if missingInWindow(u, m) {
			m.LostAudio += u.duration
		} else if lateInWindow(u, m) {
			m.LateAudio += u.duration
		}
	}
	for _, u := range lossVideo {
		if missingInWindow(u, m) {
			m.LostVideoFrames++
		} else if lateInWindow(u, m) {
			m.LateVideoFrames++
		}
	}
	m.AudioLoss.Failed = m.AudioLoss.Trusted && m.LostAudio > 0
	m.VideoLoss.Failed = m.VideoLoss.Trusted && m.LostVideoFrames > 0
	if m.AudioLoss.Failed {
		m.AudioLoss.Reasons = append(m.AudioLoss.Reasons, "sent audio never returned")
	}
	if m.VideoLoss.Failed {
		m.VideoLoss.Reasons = append(m.VideoLoss.Reasons, "sent video never returned complete")
	}
	if !classificationOnly {
		m.ContentLoss = r.contentLossEvidence(move, m, audio, video, baselineFloor)
	}
	if classificationOnly && r.prepared != nil {
		lower := min(move.Start, max(0, move.Start-baselineWindow, baselineFloor))
		upper := min(limit, move.Start+r.freshnessPolicy.EvaluationWindow())
		audio = r.indexedUnits(false, true, lower, upper)
		video = r.indexedUnits(true, true, lower, upper)
	} else {
		audio = append(slices.Clone(audio), r.repeatedAudioReturns...)
		video = append(slices.Clone(video), r.repeatedVideoReturns...)
	}
	m.Audio = legacyMeasureFreshness(audio, move.Start, m.MediaResumedAt, limit, baselineFloor, r.freshnessPolicy)
	if m.VideoExpected {
		m.Video = legacyMeasureFreshness(video, move.Start, m.MediaResumedAt, limit, baselineFloor, r.freshnessPolicy)
	} else {
		m.Video.Verdict.Trusted = true
	}
	for _, track := range r.tracks {
		freshness := &m.Audio
		if track.kind == kindVideo {
			freshness = &m.Video
		}
		for _, at := range track.unmatchedTimes {
			if at >= freshness.BaselineStart && at <= freshness.EvaluationEnd {
				freshness.Verdict.untrust("returned content identity unavailable in freshness window")
				break
			}
		}
	}
	m.ResumeVerdict = MetricVerdict{Trusted: true, Failed: m.MediaResumedAt == 0}
	if m.ResumeVerdict.Failed {
		m.ResumeVerdict.Reasons = []string{"media resume not observed before limit"}
	}
	m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict = MetricVerdict{Trusted: true}, MetricVerdict{Trusted: true}, MetricVerdict{Trusted: true}
	if m.VideoExpected {
		seenDecoded := map[time.Duration]bool{}
		var frames []frameMark
		if classificationOnly && r.prepared != nil {
			frames = r.prepared.frames
		} else {
			for _, track := range r.tracks {
				if track.video != nil {
					frames = append(frames, track.video.frames...)
				}
			}
			slices.SortFunc(frames, func(a, b frameMark) int { return cmp.Compare(a.at, b.at) })
		}
		if classificationOnly {
			first := sort.Search(len(frames), func(i int) bool { return frames[i].at >= m.MediaResumedAt })
			frames = frames[first:]
		}
		for _, frame := range frames {
			if classificationOnly && (frame.at >= limit || (m.FirstContent != "" && m.FirstNewContent != "" && m.FirstLiveFrame > 0)) {
				break
			}
			if m.MediaResumedAt == 0 || !frame.decodable || frame.at < m.MediaResumedAt || frame.at >= limit {
				continue
			}
			if frame.source.at == 0 {
				if m.FirstContent == "" {
					m.FirstContent = "unknown"
					m.FirstContentVerdict.untrust("first decoded content identity unavailable")
				}
				if m.FirstNewContent == "" {
					m.FirstNewContentVerdict.untrust("decoded content identity unavailable before first new content")
				}
				if m.FirstLiveFrame == 0 {
					m.FirstLiveVerdict.untrust("decoded content identity unavailable before first live content")
				}
				continue
			}
			if !frame.repeated && !seenDecoded[frame.source.at] {
				m.DecodedFrames++
				seenDecoded[frame.source.at] = true
			}
			prior := frame.source.returnedAt >= frame.source.at && frame.source.returnedAt > 0 && frame.source.returnedAt < move.Start
			class := classifyContent(frame, move.Start, m.WindowStart, m.MediaResumedAt, m.Video)
			if m.FirstContent == "" {
				m.FirstContent = class
				m.FirstContentPictureID = frame.pictureID
				m.FirstContentIdentity = frame.source.identity
				m.FirstContentSentAt = frame.source.at
			}
			if !frame.repeated && !prior && m.FirstNewContent == "" {
				m.FirstNewContent = class
				m.FirstNewFrame = frame.at - move.Start
				m.FirstNewContentPictureID = frame.pictureID
				m.FirstNewContentIdentity = frame.source.identity
				m.FirstNewContentSentAt = frame.source.at
			}
			if !frame.repeated && frame.source.at >= m.MediaResumedAt && m.FirstLiveFrame == 0 {
				m.FirstLiveFrame = frame.at - move.Start
			}
		}
		if m.FirstContent == "" {
			m.FirstContentVerdict.fail("no decoded frame after resume before limit")
		}
		if m.FirstNewContent == "" {
			m.FirstNewContentVerdict.fail("no decoded new content after resume before limit")
		}
		if m.FirstLiveFrame == 0 {
			m.FirstLiveVerdict.fail("no decoded frame sent after media resumed before limit")
		}
		// Latency distinguishes normal in-flight live content from outage replay.
		// Keyframe provenance and prior receipt remain known with a noisy baseline.
		if !m.Video.baselineTrusted() {
			if m.FirstContent == "live" || m.FirstContent == "outage media" {
				m.FirstContentVerdict.untrust("video baseline cannot distinguish live from outage media")
			}
			if m.FirstNewContent == "live" || m.FirstNewContent == "outage media" {
				m.FirstNewContentVerdict.untrust("video baseline cannot distinguish live from outage media")
			}
		}
	}
	if max(0, move.Start-baselineWindow) < r.historyStart {
		for _, verdict := range []*MetricVerdict{&m.AudioLoss, &m.VideoLoss, &m.Audio.Verdict, &m.Video.Verdict, &m.ResumeVerdict, &m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict} {
			verdict.untrust("event history expired before observation")
		}
	}
	if m.VideoExpected && r.recording.DecodeEvery > 0 {
		m.DecodedSamples = r.eventDecodedSamples(move.Start, limit)
		failure := r.eventDecodeFailure(move.Start, limit)
		if failure == DecodeInvalid {
			for _, verdict := range []*MetricVerdict{&m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict} {
				verdict.Trusted = true
				verdict.fail("online decoder reported invalid bitstream")
			}
		} else if m.DecodedSamples < minimumDecodedSamples || failure == DecodeUnavailable {
			for _, verdict := range []*MetricVerdict{&m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict} {
				if m.DecodedSamples < minimumDecodedSamples {
					verdict.untrust("too few online decoded samples")
				} else {
					verdict.untrust("online decode samples unavailable or dropped")
				}
			}
		}
	}
	r.failIdentityMetrics(&m)
	m.ContentLoss.Event.Audio.Trusted = m.ContentLoss.Event.Audio.Trusted && m.AudioLoss.Trusted
	m.ContentLoss.Event.Video.Trusted = m.ContentLoss.Event.Video.Trusted && m.VideoLoss.Trusted
	m.updateJudgment()
	return m
}

func TestDistributedReportEvidenceEquivalent(t *testing.T) {
	for _, bound := range []time.Duration{0, time.Millisecond, 25 * time.Millisecond} {
		for _, variant := range []string{"normal", "missing", "unknown", "reversed", "repeated"} {
			t.Run(fmt.Sprint(bound, "/", variant), func(t *testing.T) {
				r, c := denseDistributedCall(t, true, 50*time.Millisecond, bound)
				recordDenseEvent(t, r, c, "move", "move", 5*time.Second, 5030*time.Millisecond)
				recordDenseEvent(t, r, c, "kill", "takeover", 10*time.Second, 10500*time.Millisecond)
				switch variant {
				case "missing":
					u := r.sentAudioUnits["a250"]
					u.returnedAt = 0
					r.sentAudioUnits["a250"] = u
					f := r.sentVideoFrames[300]
					f.returnedAt = 0
					r.sentVideoFrames[300] = f
				case "unknown":
					r.contentIdentityErrors = 1
					r.tracks[0].unmatchedTimes = []time.Duration{5100 * time.Millisecond}
				case "repeated":
					addDenseRepeatedReturns(r)
				case "reversed":
					slices.Reverse(r.moves)
				}
				oldView := r.measurementSnapshot(r.hungUpAt, len(r.moves))
				newView := r.measurementSnapshot(r.hungUpAt, len(r.moves))
				expected := oldView.legacyMeasureMoves(r.hungUpAt, len(r.moves))
				actual := newView.measureMoves(r.hungUpAt, len(r.moves))
				require.Equal(t, expected, actual)
				a, ca := JudgeDistributedTrials(expected)
				b, cb := JudgeDistributedTrials(actual)
				require.Equal(t, a, b)
				require.Equal(t, ca, cb)
			})
		}
	}
}

// The full retained-history oracle deliberately allocates like the old code.
// Opt in alongside the cost reproduction rather than doing that in every CI run.
func TestDistributedReportBudgetEvidenceEquivalent(t *testing.T) {
	if os.Getenv("RELAIS_MEASURE_CALLER_COST") != "1" {
		t.Skip("opt-in full work-budget legacy oracle")
	}
	r := costCalls(t, 30)[0].rec
	// Dominated floors now make the original narrow fixture complete. Widen
	// only this cap oracle's worker bounds so it still tests actual exhaustion.
	r.distributed.WorkerOffset = OffsetInterval{Lower: -250 * time.Millisecond, Upper: 250 * time.Millisecond}
	oldView := r.measurementSnapshot(r.hungUpAt, len(r.moves))
	newView := r.measurementSnapshot(r.hungUpAt, len(r.moves))
	expected := oldView.legacyMeasureMoves(r.hungUpAt, len(r.moves))
	actual := newView.measureMoves(r.hungUpAt, len(r.moves))
	require.Equal(t, expected, actual)
	capped := 0
	for _, m := range actual {
		if m.Distributed.Evaluations == maxClassificationEvaluations {
			capped++
		}
	}
	require.Positive(t, capped, "exercise the real 4096-cell limit")
	t.Logf("identical full evidence for 30 events, including %d capped events", capped)
}
