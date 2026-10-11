package callharness

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

const (
	baselineWindow           = time.Second
	freshnessDeadline        = 2 * time.Second
	freshnessStabilityWindow = 3 * time.Second
	contentWindowMargin      = 250 * time.Millisecond
	contentSettleWindow      = 500 * time.Millisecond
	baselineMinSamples       = 10
	baselineSpreadLimit      = 20 * time.Millisecond
	freshnessTolerance       = 10 * time.Millisecond
	freshnessSustainedUnits  = 3
)

// MetricVerdict separates trust from an observed failure. Unknown metrics must
// not mask failures in other trusted metrics. Reasons explains either result.
type MetricVerdict struct {
	Trusted, Failed bool
	Reasons         []string
}

// EventMeasurement separates interruption, missing content, content age and
// decoded recovery. All times use the caller clock, not RTP timestamps.
// Pause retains the existing per-kind MoveTrackReport.Gap.
// Loss counts successful sends in [Start-250ms, max(End,MediaResumedAt)+250ms).
// Lost means no valid complete return by ObservedUntil (call hangup). Returns
// after SettledAt are late, separately from loss. Hangup before SettledAt makes
// loss unknown. Duplicate/reordered returns count once. Nearby event windows
// can overlap; never sum their losses. Video loss does not depend on decoding.
// FirstContent classifies the first decoded frame after resume. Prior complete
// receipt before Start means cache, regardless of who replays the frame.
// FirstNewContent skips those prior receipts. Its classes are keyframe (PLI/FIR
// requested at/after Start), outage media (sent from WindowStart until resume
// with latency above baseline p95+tolerance), or live (normal-age content,
// including natural keyframes and ordinary in-flight frames).
// FirstNewFrame is event-to-decode for that new content. FirstLiveFrame retains
// event-to-decode for the first frame sent at/after MediaResumedAt.
// Each verdict applies to one metric. Inconclusive is informational: at least
// one metric is unknown. Failed means at least one trusted metric failed.
// Missing resume/decoded content/recovery are failures, not uncertainty.
type EventMeasurement struct {
	VideoExpected                                                 bool
	AudioPause, VideoPause                                        time.Duration
	WindowStart, WindowEnd, SettledAt, ObservedUntil              time.Duration
	SettleWindow                                                  time.Duration
	LostAudio, LateAudio                                          time.Duration
	LostVideoFrames, LateVideoFrames                              int
	AudioLoss, VideoLoss                                          MetricVerdict
	Audio, Video                                                  FreshnessReport
	MediaResumedAt                                                time.Duration
	ResumeVerdict                                                 MetricVerdict
	FirstLiveFrame, FirstNewFrame                                 time.Duration
	FirstContent, FirstNewContent                                 string
	FirstContentPictureID, FirstNewContentPictureID               uint16
	FirstContentSentAt, FirstNewContentSentAt                     time.Duration
	FirstContentVerdict, FirstNewContentVerdict, FirstLiveVerdict MetricVerdict
	DecodedFrames                                                 int
	Inconclusive, Failed                                          bool
	Reasons                                                       []string
}

// FreshnessReport uses one latency sample per unique returned content unit:
// arrival minus send time (video arrival is completion of all frame packets).
// BaselineMedian and BaselineP95 use units received in the preceding 1s whose
// sends also precede the event. After a preceding takeover, the window starts
// no earlier than that event's measured audio/video recovery, or its end/resume
// if recovery failed. A failed event cannot truncate every later baseline.
// BaselineStart records the actual window start. Ten samples are required;
// p95-minus-median must be <=20ms. PeakLatency includes every post-event cache/outage return,
// even repeated content. Repeats cannot add samples or confirm recovery, but
// a stale repeat interrupts a run of fresh units.
// Recovery is evaluated only through EvaluationEnd: event start plus
// RecoveryLimit (default 2s) and StabilityWindow (default 3s), clipped at the
// next event/hangup. Later units do not affect this event, including its peak.
// BackToBaseline is the start of the last fresh run confirmed by three unique
// units. Any post-resume receipt sent before resume breaks the run, regardless
// of latency. Three consecutive late live units also break it. Shorter live
// lateness streaks do not break recovery; JitterUnits and JitterMaxExcess report
// their count and maximum excess over baseline p95+tolerance. Repeats cannot
// confirm recovery. LastAboveTolerance records the last above-threshold unit.
// No recovery at window end, or recovery after RecoveryLimit, fails a trusted
// metric. FirstBaseline* retain the first call baseline; LastingLag flags a
// relative median/p95 above that trusted baseline by more than tolerance.
// Freshness measures content age independently of whether it decoded.
type FreshnessReport struct {
	Verdict                                       MetricVerdict
	RecoveryLimit, StabilityWindow, EvaluationEnd time.Duration
	JitterUnits                                   int
	JitterMaxExcess                               time.Duration
	FirstBaselineTrusted                          bool
	LastAboveTolerance                            time.Duration
	FirstBaselineSamples                          int
	FirstBaselineMedian, FirstBaselineP95         time.Duration
	LastingLag                                    bool
	BaselineStart                                 time.Duration
	BaselineSamples, PostEventSamples             int
	BaselineMedian, BaselineP95                   time.Duration
	PeakLatency, PeakAboveBaseline                time.Duration
	BackToBaseline                                time.Duration
	Recovered                                     bool
	Tolerance                                     time.Duration
	SustainedUnits                                int
	SpreadLimit                                   time.Duration
}

// FreshnessPolicy configures event recovery attribution. Nonpositive values
// use the defaults: 2s to recover followed by 3s of stability observation.
type FreshnessPolicy struct {
	RecoveryLimit, StabilityWindow time.Duration
}

func (p FreshnessPolicy) defaults() FreshnessPolicy {
	if p.RecoveryLimit <= 0 {
		p.RecoveryLimit = freshnessDeadline
	}
	if p.StabilityWindow <= 0 {
		p.StabilityWindow = freshnessStabilityWindow
	}
	return p
}

type contentUnit struct {
	at, duration, returnedAt time.Duration
	identity                 string
	written                  bool
	replayed                 bool
}

type videoAssemblyKey struct {
	track     *trackRecord
	pictureID uint16
	timestamp uint32
}

type contentPacketSpan struct{ first, last uint16 }

func (s contentPacketSpan) contains(seq uint16) bool {
	return uint16(seq-s.first) <= uint16(s.last-s.first)
}

type contentAssembly struct {
	parts        map[uint16][]byte
	starts, ends map[uint16]bool
	completed    []contentPacketSpan
}

// completeSpan finds an exact source payload in one contiguous transmission.
// A replay may keep its timestamp but use new sequence numbers. Retain start
// and marker candidates so its fragments cannot be mixed with a partial old
// transmission, even when the new marker arrives first.
func (a *contentAssembly) completeSpan(expected string) (contentPacketSpan, bool) {
	for first := range a.starts {
		for last := range a.ends {
			n := int(uint16(last-first)) + 1
			if n > len(a.parts) {
				continue
			}
			var data []byte
			complete := true
			for i := 0; i < n; i++ {
				part, ok := a.parts[first+uint16(i)] //nolint:gosec // RTP sequence wraps intentionally
				if !ok {
					complete = false
					break
				}
				data = append(data, part...)
				if len(data) > len(expected) {
					complete = false
					break
				}
			}
			if complete && string(data) == expected {
				return contentPacketSpan{first: first, last: last}, true
			}
		}
	}
	return contentPacketSpan{}, false
}

func (a *contentAssembly) finish(span contentPacketSpan) {
	a.completed = append(a.completed, span)
	for seq := range a.parts {
		if span.contains(seq) {
			delete(a.parts, seq)
		}
	}
	for seq := range a.starts {
		if span.contains(seq) {
			delete(a.starts, seq)
		}
	}
	for seq := range a.ends {
		if span.contains(seq) {
			delete(a.ends, seq)
		}
	}
	if len(a.parts) == 0 {
		a.parts, a.starts, a.ends = nil, nil, nil
	}
}

// returnedVideo measures completeness separately from the arrival-order decoder
// used by phase 1. It accepts reordered fragments and exact duplicates, and
// checks the reassembled payload against the send identified by PictureID.
// Assemblies retain at most received packet state, proportional to the existing
// recorder. Wrap-safe and bounded long-run recording belongs to #33.
func (r *recorder) returnedVideo(track *trackRecord, pkt *rtp.Packet, at time.Duration) {
	var desc codecs.VP8Packet
	payload, err := desc.Unmarshal(pkt.Payload)
	// Pion omits the descriptor extension for PictureID 0. Exact payload
	// comparison still disambiguates that first frame in these bounded calls.
	if err != nil {
		return
	}
	source, ok := r.sentVideoFrames[desc.PictureID]
	if !ok {
		return
	}
	key := videoAssemblyKey{track: track, pictureID: desc.PictureID, timestamp: pkt.Timestamp}
	a := r.contentAssemblies[key]
	if a == nil {
		a = &contentAssembly{parts: make(map[uint16][]byte), starts: make(map[uint16]bool), ends: make(map[uint16]bool)}
		r.contentAssemblies[key] = a
	}
	for _, span := range a.completed {
		if span.contains(pkt.SequenceNumber) {
			return
		}
	}
	if a.parts == nil {
		a.parts = make(map[uint16][]byte)
		a.starts, a.ends = make(map[uint16]bool), make(map[uint16]bool)
	}
	if previous, ok := a.parts[pkt.SequenceNumber]; ok {
		if !bytes.Equal(previous, payload) {
			r.contentIdentityErrors++
		}
		return
	}
	a.parts[pkt.SequenceNumber] = bytes.Clone(payload)
	if desc.S == 1 && desc.PID == 0 {
		a.starts[pkt.SequenceNumber] = true
	}
	if pkt.Marker {
		a.ends[pkt.SequenceNumber] = true
	}
	span, complete := a.completeSpan(source.data)
	if !complete {
		return
	}
	if source.returnedAt == 0 {
		source.returnedAt = at
		r.sentVideoFrames[desc.PictureID] = source
	} else {
		unit := source.unit()
		unit.returnedAt, unit.identity, unit.replayed = at, fmt.Sprint(desc.PictureID), true
		r.repeatedVideoReturns = append(r.repeatedVideoReturns, unit)
	}
	a.finish(span)
}

func (r *recorder) eventMeasurement(move MoveReport, limit, baselineFloor time.Duration) EventMeasurement {
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
			for _, at := range track.arrivals {
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
	if r.contentIdentityErrors > 0 {
		m.VideoLoss.untrust("conflicting video content identities")
	}
	if r.sentVideo.Frames > 32768 {
		m.VideoLoss.untrust("video PictureID wrapped; attribution requires #33")
	}
	var audio, video []contentUnit
	for identity, unit := range r.sentAudioUnits {
		if !unit.written {
			continue
		}
		unit.identity = identity
		audio = append(audio, unit)
		if missingInWindow(unit, m) {
			m.LostAudio += unit.duration
		} else if lateInWindow(unit, m) {
			m.LateAudio += unit.duration
		}
	}
	for identity, source := range r.sentVideoFrames {
		if !source.written {
			continue
		}
		unit := source.unit()
		unit.identity = fmt.Sprint(identity)
		video = append(video, unit)
		if missingInWindow(unit, m) {
			m.LostVideoFrames++
		} else if lateInWindow(unit, m) {
			m.LateVideoFrames++
		}
	}
	if len(audio) == 0 {
		m.AudioLoss.untrust("no identified sent audio")
	}
	if m.VideoExpected && len(video) == 0 {
		m.VideoLoss.untrust("no identified sent video")
	}
	m.AudioLoss.Failed = m.AudioLoss.Trusted && m.LostAudio > 0
	m.VideoLoss.Failed = m.VideoLoss.Trusted && m.LostVideoFrames > 0
	if m.AudioLoss.Failed {
		m.AudioLoss.Reasons = append(m.AudioLoss.Reasons, "sent audio never returned")
	}
	if m.VideoLoss.Failed {
		m.VideoLoss.Reasons = append(m.VideoLoss.Reasons, "sent video never returned complete")
	}
	audio = append(audio, r.repeatedAudioReturns...)
	video = append(video, r.repeatedVideoReturns...)
	m.Audio = measureFreshness(audio, move.Start, m.MediaResumedAt, limit, baselineFloor, r.freshnessPolicy)
	if m.VideoExpected {
		m.Video = measureFreshness(video, move.Start, m.MediaResumedAt, limit, baselineFloor, r.freshnessPolicy)
		if r.contentIdentityErrors > 0 || r.sentVideo.Frames > 32768 {
			m.Video.Verdict.untrust("video identity attribution unavailable")
		}
	} else {
		m.Video.Verdict.Trusted = true
	}
	m.ResumeVerdict = MetricVerdict{Trusted: true, Failed: m.MediaResumedAt == 0}
	if m.ResumeVerdict.Failed {
		m.ResumeVerdict.Reasons = []string{"media resume not observed before limit"}
	}
	m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict = MetricVerdict{Trusted: true}, MetricVerdict{Trusted: true}, MetricVerdict{Trusted: true}
	if m.VideoExpected {
		seenDecoded := map[uint16]bool{}
		var frames []frameMark
		for _, track := range r.tracks {
			if track.video != nil {
				frames = append(frames, track.video.frames...)
			}
		}
		slices.SortFunc(frames, func(a, b frameMark) int { return cmp.Compare(a.at, b.at) })
		for _, frame := range frames {
			if m.MediaResumedAt == 0 || !frame.decodable || frame.at < m.MediaResumedAt || frame.at >= limit || frame.source.at == 0 {
				continue
			}
			if !seenDecoded[frame.pictureID] {
				m.DecodedFrames++
				seenDecoded[frame.pictureID] = true
			}
			prior := frame.source.returnedAt >= frame.source.at && frame.source.returnedAt > 0 && frame.source.returnedAt < move.Start
			class := classifyContent(frame, move.Start, m.WindowStart, m.MediaResumedAt, m.Video)
			if m.FirstContent == "" {
				m.FirstContent = class
				m.FirstContentPictureID = frame.pictureID
				m.FirstContentSentAt = frame.source.at
			}
			if !prior && m.FirstNewContent == "" {
				m.FirstNewContent = class
				m.FirstNewFrame = frame.at - move.Start
				m.FirstNewContentPictureID = frame.pictureID
				m.FirstNewContentSentAt = frame.source.at
			}
			if frame.source.at >= m.MediaResumedAt && m.FirstLiveFrame == 0 {
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
		if r.contentIdentityErrors > 0 || r.sentVideo.Frames > 32768 {
			for _, reason := range []string{"video identity attribution unavailable"} {
				m.FirstContentVerdict.untrust(reason)
				m.FirstNewContentVerdict.untrust(reason)
				m.FirstLiveVerdict.untrust(reason)
			}
		}
		// Latency distinguishes normal in-flight live content from outage replay.
		// Keyframe provenance and prior receipt remain known with a noisy baseline.
		if !m.Video.Verdict.Trusted {
			if m.FirstContent == "live" || m.FirstContent == "outage media" {
				m.FirstContentVerdict.untrust("video baseline cannot distinguish live from outage media")
			}
			if m.FirstNewContent == "live" || m.FirstNewContent == "outage media" {
				m.FirstNewContentVerdict.untrust("video baseline cannot distinguish live from outage media")
			}
		}
	}
	m.updateJudgment()
	return m
}

func classifyContent(frame frameMark, start, windowStart, resumed time.Duration, f FreshnessReport) string {
	source := frame.source
	if source.returnedAt > 0 && source.returnedAt >= source.at && source.returnedAt < start {
		return "cache"
	}
	if source.pli && source.requestedAt >= start {
		return "keyframe"
	}
	arrived := frame.receivedAt
	if arrived == 0 {
		arrived = frame.at
		if source.returnedAt >= resumed && source.returnedAt <= frame.at {
			arrived = source.returnedAt
		}
	}
	if source.at >= windowStart && source.at < resumed && arrived-source.at > f.BaselineP95+f.Tolerance {
		return "outage media"
	}
	return "live"
}

func missingInWindow(unit contentUnit, m EventMeasurement) bool {
	until := m.ObservedUntil
	if until == 0 {
		until = m.SettledAt
	}
	return unit.at >= m.WindowStart && unit.at < m.WindowEnd && (unit.returnedAt == 0 || unit.returnedAt < unit.at || unit.returnedAt > until)
}

func lateInWindow(unit contentUnit, m EventMeasurement) bool {
	return unit.at >= m.WindowStart && unit.at < m.WindowEnd && unit.returnedAt >= unit.at && unit.returnedAt > m.SettledAt && unit.returnedAt <= m.ObservedUntil
}

func (v *MetricVerdict) untrust(reason string) {
	v.Trusted = false
	v.Reasons = append(v.Reasons, reason)
}
func (v *MetricVerdict) fail(reason string) { v.Failed = true; v.Reasons = append(v.Reasons, reason) }
func (v MetricVerdict) Summary() string {
	state := "pass"
	if !v.Trusted {
		state = "inconclusive"
	} else if v.Failed {
		state = "fail"
	}
	if len(v.Reasons) > 0 {
		state += " (" + strings.Join(v.Reasons, ", ") + ")"
	}
	return state
}

// MinimumConclusivePercent applies to every metric a scenario asserts. Unknown
// events do not count as passes; at least 80% must have trustworthy judgments.
const MinimumConclusivePercent = 80

func EnoughConclusive(conclusive, total int) bool {
	return total > 0 && conclusive*100 >= total*MinimumConclusivePercent
}
func (m EventMeasurement) LossConclusive() bool {
	return m.SettleWindow > 0 && m.AudioLoss.Trusted && m.VideoLoss.Trusted
}
func (m EventMeasurement) Conclusive() bool {
	if m.SettleWindow <= 0 {
		return false
	}
	for _, v := range []MetricVerdict{m.AudioLoss, m.VideoLoss, m.Audio.Verdict, m.Video.Verdict, m.ResumeVerdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
		if !v.Trusted {
			return false
		}
	}
	return true
}

// RecoveryFailed excludes loss, which is expected in today's unbuffered crashes.
func (m EventMeasurement) RecoveryFailed() bool {
	for _, v := range []MetricVerdict{m.ResumeVerdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict, m.Audio.Verdict, m.Video.Verdict} {
		if v.Trusted && v.Failed {
			return true
		}
	}
	return false
}
func (m *EventMeasurement) updateJudgment() {
	m.Inconclusive, m.Failed = false, false
	m.Reasons = nil
	for _, metric := range []struct {
		name string
		v    MetricVerdict
	}{{"audio loss", m.AudioLoss}, {"video loss", m.VideoLoss}, {"audio freshness", m.Audio.Verdict}, {"video freshness", m.Video.Verdict}, {"resume", m.ResumeVerdict}, {"first content", m.FirstContentVerdict}, {"first new content", m.FirstNewContentVerdict}, {"first live", m.FirstLiveVerdict}} {
		m.Inconclusive = m.Inconclusive || !metric.v.Trusted
		m.Failed = m.Failed || metric.v.Trusted && metric.v.Failed
		for _, reason := range metric.v.Reasons {
			m.Reasons = append(m.Reasons, metric.name+": "+reason)
		}
	}
}

func measureFreshness(units []contentUnit, start, resumed, limit, baselineFloor time.Duration, policies ...FreshnessPolicy) FreshnessReport {
	policy := FreshnessPolicy{}.defaults()
	if len(policies) > 0 {
		policy = policies[0].defaults()
	}
	f := FreshnessReport{RecoveryLimit: policy.RecoveryLimit, StabilityWindow: policy.StabilityWindow, EvaluationEnd: min(limit, start+policy.RecoveryLimit+policy.StabilityWindow), BaselineStart: max(0, start-baselineWindow, baselineFloor), Tolerance: freshnessTolerance, SustainedUnits: freshnessSustainedUnits, SpreadLimit: baselineSpreadLimit}
	var baseline []time.Duration
	var post []contentUnit
	seen := map[string]bool{}
	seenPost := map[string]bool{}
	slices.SortFunc(units, func(a, b contentUnit) int { return cmp.Compare(a.returnedAt, b.returnedAt) })
	for i, unit := range units {
		if unit.returnedAt == 0 || unit.returnedAt < unit.at {
			continue
		}
		if unit.identity == "" {
			unit.identity = fmt.Sprint(i)
		}
		if !seen[unit.identity] && !unit.replayed && unit.returnedAt >= f.BaselineStart && unit.returnedAt < start && unit.at < start {
			baseline = append(baseline, unit.returnedAt-unit.at)
		}
		seen[unit.identity] = true
		if unit.returnedAt >= start && unit.returnedAt <= f.EvaluationEnd {
			f.PeakLatency = max(f.PeakLatency, unit.returnedAt-unit.at)
			post = append(post, unit)
			seenPost[unit.identity] = true
		}
	}
	f.BaselineSamples, f.PostEventSamples = len(baseline), len(seenPost)
	slices.Sort(baseline)
	if len(baseline) > 0 {
		f.BaselineMedian = baseline[len(baseline)/2]
		f.BaselineP95 = baseline[(95*len(baseline)+99)/100-1]
	}
	f.FirstBaselineSamples, f.FirstBaselineMedian, f.FirstBaselineP95 = f.BaselineSamples, f.BaselineMedian, f.BaselineP95
	var runStart time.Duration
	sustained, lateLive := 0, 0
	var lateExcess time.Duration
	flushJitter := func() {
		if lateLive > 0 && lateLive < f.SustainedUnits {
			f.JitterUnits += lateLive
			f.JitterMaxExcess = max(f.JitterMaxExcess, lateExcess)
		}
		lateLive, lateExcess = 0, 0
	}
	counted := map[string]bool{}
	for _, unit := range post {
		latency := unit.returnedAt - unit.at
		excess := latency - f.BaselineP95 - f.Tolerance
		if excess > 0 {
			f.LastAboveTolerance = unit.returnedAt - start
		}
		if resumed == 0 || unit.returnedAt < resumed {
			continue
		}
		switch {
		case unit.at < resumed:
			flushJitter()
			sustained, runStart = 0, 0
		case excess > 0:
			lateLive++
			lateExcess = max(lateExcess, excess)
			if lateLive >= f.SustainedUnits {
				sustained, runStart = 0, 0
			}
		default:
			flushJitter()
			if !counted[unit.identity] && !unit.replayed {
				if sustained == 0 {
					runStart = unit.returnedAt - start
				}
				sustained++
			}
		}
		counted[unit.identity] = true
	}
	flushJitter()
	f.Recovered = resumed > 0 && sustained >= f.SustainedUnits
	if f.Recovered {
		f.BackToBaseline = runStart
	}
	f.PeakAboveBaseline = max(0, f.PeakLatency-f.BaselineP95)
	f.FirstBaselineTrusted = f.BaselineSamples >= baselineMinSamples && f.BaselineP95-f.BaselineMedian <= f.SpreadLimit
	f.updateVerdict()
	return f
}

func (f *FreshnessReport) updateVerdict() {
	f.Verdict = MetricVerdict{Trusted: true}
	if f.BaselineSamples < baselineMinSamples {
		f.Verdict.Trusted = false
		f.Verdict.Reasons = append(f.Verdict.Reasons, fmt.Sprintf("baseline has %d samples (need %d)", f.BaselineSamples, baselineMinSamples))
	}
	if f.BaselineP95-f.BaselineMedian > f.SpreadLimit {
		f.Verdict.Trusted = false
		f.Verdict.Reasons = append(f.Verdict.Reasons, "baseline p95-minus-median exceeds 20ms")
	}
	if f.Verdict.Trusted {
		switch {
		case !f.Recovered:
			f.Verdict.Failed = true
			f.Verdict.Reasons = append(f.Verdict.Reasons, "freshness not recovered before limit (need 3 sustained units)")
		case f.BackToBaseline > f.RecoveryLimit:
			f.Verdict.Failed = true
			f.Verdict.Reasons = append(f.Verdict.Reasons, fmt.Sprintf("freshness recovery exceeds %s", f.RecoveryLimit))
		}
		f.LastingLag = f.FirstBaselineTrusted && f.FirstBaselineSamples >= baselineMinSamples && (f.BaselineMedian > f.FirstBaselineMedian+f.Tolerance || f.BaselineP95 > f.FirstBaselineP95+f.Tolerance)
		if f.LastingLag {
			f.Verdict.Failed = true
			f.Verdict.Reasons = append(f.Verdict.Reasons, "relative baseline exceeds first baseline plus tolerance (lasting lag)")
		}
	}
}

func freshnessReasons(kind string, f FreshnessReport) []string {
	var reasons []string
	for _, reason := range f.Verdict.Reasons {
		reasons = append(reasons, kind+" "+reason)
	}
	return reasons
}

// Summary prints an event record with explicit units and measurement policy.
func (m EventMeasurement) Summary() string {
	if m.SettleWindow == 0 {
		return "inconclusive: event measurement not available"
	}
	state := "conclusive"
	if m.Failed {
		state = "failed"
	}
	if m.Inconclusive {
		state += "; inconclusive: " + strings.Join(m.Reasons, "; ")
	}
	return fmt.Sprintf("pause audio=%s video=%s; lost audio=%s video=%d frames; window=[%s,%s) settle=%s; freshness audio{%s} video{%s}; first_live=%s first_content=%s picture_id=%d sent_at=%s; first_new=%s new_decode=%s new_picture_id=%d new_sent_at=%s; late audio=%s video=%d frames observed_until=%s; trust audio_loss=%t video_loss=%t audio_fresh=%t video_fresh=%t first=%t new=%t live=%t resume=%t; verdict audio_loss=%s video_loss=%s first=%s new=%s live=%s resume=%s; %s",
		m.AudioPause, m.VideoPause, m.LostAudio, m.LostVideoFrames, m.WindowStart, m.WindowEnd, m.SettleWindow,
		m.Audio.summary(), m.Video.summary(), observedDuration(m.FirstLiveFrame, m.FirstLiveFrame > 0), m.FirstContent, m.FirstContentPictureID, m.FirstContentSentAt, m.FirstNewContent, observedDuration(m.FirstNewFrame, m.FirstNewContent != ""), m.FirstNewContentPictureID, m.FirstNewContentSentAt, m.LateAudio, m.LateVideoFrames, m.ObservedUntil, m.AudioLoss.Trusted, m.VideoLoss.Trusted, m.Audio.Verdict.Trusted, m.Video.Verdict.Trusted, m.FirstContentVerdict.Trusted, m.FirstNewContentVerdict.Trusted, m.FirstLiveVerdict.Trusted, m.ResumeVerdict.Trusted, m.AudioLoss.Summary(), m.VideoLoss.Summary(), m.FirstContentVerdict.Summary(), m.FirstNewContentVerdict.Summary(), m.FirstLiveVerdict.Summary(), m.ResumeVerdict.Summary(), state)
}

func (f FreshnessReport) summary() string {
	return fmt.Sprintf("baseline_n=%d median=%s p95=%s peak=%s above_p95=%s back=%s tolerance=%s sustained=%d spread_limit=%s baseline_start=%s first_n=%d first_median=%s first_p95=%s first_trusted=%t lasting_lag=%t last_stale=%s recovery_limit=%s stability_window=%s evaluation_end=%s jitter_units=%d jitter_max_excess=%s verdict=%s", f.BaselineSamples, f.BaselineMedian, f.BaselineP95, f.PeakLatency, f.PeakAboveBaseline, observedDuration(f.BackToBaseline, f.Recovered), f.Tolerance, f.SustainedUnits, f.SpreadLimit, f.BaselineStart, f.FirstBaselineSamples, f.FirstBaselineMedian, f.FirstBaselineP95, f.FirstBaselineTrusted, f.LastingLag, observedDuration(f.LastAboveTolerance, f.LastAboveTolerance > 0), f.RecoveryLimit, f.StabilityWindow, f.EvaluationEnd, f.JitterUnits, f.JitterMaxExcess, f.Verdict.Summary())
}

func observedDuration(d time.Duration, observed bool) string {
	if !observed {
		return "not observed"
	}
	return d.String()
}
