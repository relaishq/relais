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
	baselineWindow          = time.Second
	contentWindowMargin     = 250 * time.Millisecond
	contentSettleWindow     = 500 * time.Millisecond
	baselineMinSamples      = 10
	baselineSpreadLimit     = 20 * time.Millisecond
	freshnessTolerance      = 10 * time.Millisecond
	freshnessSustainedUnits = 3
)

// EventMeasurement separates interruption, missing content, content age and
// decoded recovery. All times use the caller's clock, not RTP timestamps.
// Pause retains MoveTrackReport.Gap (audio and video separately).
// Lost content counts successful sends in [Start-250ms, End+250ms), observed
// until WindowEnd+500ms. Late and duplicate deliveries count only once. Windows
// can overlap for nearby events; their losses must not be summed.
// Video loss means a sent frame never returned complete, even if other complete
// returned frames could not decode because a reference was lost.
// FirstLiveFrame is event-to-decode for the first frame sent at/after
// MediaResumedAt. FirstContent classifies the first decoded frame received
// entirely after resume: cache (sent before the event), outage media (sent
// during the outage, excluding request responses), keyframe (a PLI/FIR
// response, including requests received after the event whose response was
// sent before observed resume), or live
// (ordinary post-resume content, including natural keyframes).
// Inconclusive measurements keep their observations, but Reasons explains why
// they must not be used as a pass/fail judgment.
type EventMeasurement struct {
	AudioPause, VideoPause            time.Duration
	WindowStart, WindowEnd, SettledAt time.Duration
	SettleWindow                      time.Duration
	LostAudio                         time.Duration
	LostVideoFrames                   int
	Audio, Video                      FreshnessReport
	MediaResumedAt                    time.Duration
	FirstLiveFrame                    time.Duration
	FirstContent                      string
	FirstContentPictureID             uint16
	FirstContentSentAt                time.Duration
	DecodedFrames                     int
	Inconclusive                      bool
	Reasons                           []string
}

// FreshnessReport uses one latency sample per unique returned content unit:
// arrival minus send time (video arrival is completion of all frame packets).
// BaselineMedian and BaselineP95 use units received in the preceding 1s whose
// sends also precede the event. After a preceding takeover, the window starts
// no earlier than that event's measured audio/video freshness recovery. This
// excludes its cache burst without relaxing the sample count or spread limit.
// BaselineStart records the actual window start. Ten samples are required;
// p95-minus-median must be <=20ms. PeakLatency includes every post-event cache/outage return,
// even repeated content. Repeats cannot add samples or confirm recovery, but
// a stale repeat interrupts a run of fresh units.
// BackToBaseline is event-to-arrival of the third consecutive unique unit
// within baseline p95+10ms, after media resumes. Recovered distinguishes an
// observed recovery from a zero/unavailable duration. This is content age,
// independent of whether the decoder could display the frame.
type FreshnessReport struct {
	BaselineStart                     time.Duration
	BaselineSamples, PostEventSamples int
	BaselineMedian, BaselineP95       time.Duration
	PeakLatency, PeakAboveBaseline    time.Duration
	BackToBaseline                    time.Duration
	Recovered                         bool
	Tolerance                         time.Duration
	SustainedUnits                    int
	SpreadLimit                       time.Duration
}

type contentUnit struct {
	at, duration, returnedAt time.Duration
	identity                 string
	written                  bool
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
		unit.returnedAt, unit.identity = at, fmt.Sprint(desc.PictureID)
		r.repeatedVideoReturns = append(r.repeatedVideoReturns, unit)
	}
	a.finish(span)
}

func (r *recorder) eventMeasurement(move MoveReport, limit, baselineFloor time.Duration) EventMeasurement {
	m := EventMeasurement{
		WindowStart:    max(0, move.Start-contentWindowMargin),
		WindowEnd:      move.End + contentWindowMargin,
		SettleWindow:   contentSettleWindow,
		MediaResumedAt: move.Recovery.MediaResumedAt,
	}
	m.SettledAt = m.WindowEnd + m.SettleWindow
	if r.hungUpAt < m.SettledAt {
		m.Reasons = append(m.Reasons, "content settle window truncated by hangup")
	}
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
	var audio, video []contentUnit
	for identity, unit := range r.sentAudioUnits {
		unit.identity = identity
		if unit.written {
			audio = append(audio, unit)
			if missingInWindow(unit, m) {
				m.LostAudio += unit.duration
			}
		}
	}
	for identity, source := range r.sentVideoFrames {
		if source.written {
			unit := source.unit()
			unit.identity = fmt.Sprint(identity)
			video = append(video, unit)
			if missingInWindow(source.unit(), m) {
				m.LostVideoFrames++
			}
		}
	}
	audio = append(audio, r.repeatedAudioReturns...)
	video = append(video, r.repeatedVideoReturns...)
	m.Audio = measureFreshness(audio, move.Start, m.MediaResumedAt, limit, baselineFloor)
	m.Reasons = append(m.Reasons, freshnessReasons("audio", m.Audio)...)
	if r.sentVideo.Frames > 0 {
		m.Video = measureFreshness(video, move.Start, m.MediaResumedAt, limit, baselineFloor)
		m.Reasons = append(m.Reasons, freshnessReasons("video", m.Video)...)
		if r.sentVideo.Frames > 32768 {
			m.Reasons = append(m.Reasons, "video PictureID wrapped; attribution requires #33")
		}
		seenDecoded := map[uint16]bool{}
		for _, track := range r.tracks {
			if track.video == nil {
				continue
			}
			for _, frame := range track.video.frames {
				if m.MediaResumedAt == 0 || !frame.decodable || frame.at < m.MediaResumedAt || frame.firstArrival < m.MediaResumedAt || frame.at >= limit || frame.source.at == 0 {
					continue
				}
				if !seenDecoded[frame.pictureID] {
					m.DecodedFrames++
					seenDecoded[frame.pictureID] = true
				}
				if m.FirstContent == "" {
					m.FirstContentPictureID, m.FirstContentSentAt = frame.pictureID, frame.source.at
					switch {
					case frame.source.at < move.Start:
						m.FirstContent = "cache"
					case frame.source.pli:
						m.FirstContent = "keyframe"
					case frame.source.at < m.MediaResumedAt:
						m.FirstContent = "outage media"
					default:
						m.FirstContent = "live"
					}
				}
				if frame.source.at >= m.MediaResumedAt && m.FirstLiveFrame == 0 {
					m.FirstLiveFrame = frame.at - move.Start
				}
			}
		}
		if m.DecodedFrames < freshnessSustainedUnits {
			m.Reasons = append(m.Reasons, "too few decoded post-resume video frames (need 3)")
		}
		if m.FirstLiveFrame == 0 {
			m.Reasons = append(m.Reasons, "no decoded frame sent after media resumed")
		}
	}
	if m.MediaResumedAt == 0 {
		m.Reasons = append(m.Reasons, "media resume not observed")
	}
	if r.contentIdentityErrors > 0 {
		m.Reasons = append(m.Reasons, "conflicting video content identities")
	}
	m.Inconclusive = len(m.Reasons) > 0
	return m
}

func missingInWindow(unit contentUnit, m EventMeasurement) bool {
	return unit.at >= m.WindowStart && unit.at < m.WindowEnd && (unit.returnedAt == 0 || unit.returnedAt > m.SettledAt)
}

func measureFreshness(units []contentUnit, start, resumed, limit, baselineFloor time.Duration) FreshnessReport {
	f := FreshnessReport{BaselineStart: max(0, start-baselineWindow, baselineFloor), Tolerance: freshnessTolerance, SustainedUnits: freshnessSustainedUnits, SpreadLimit: baselineSpreadLimit}
	var baseline []time.Duration
	var post []contentUnit
	seenBaseline := map[string]bool{}
	seenPost := map[string]bool{}
	slices.SortFunc(units, func(a, b contentUnit) int { return cmp.Compare(a.returnedAt, b.returnedAt) })
	for i, unit := range units {
		identity := unit.identity
		if identity == "" {
			identity = fmt.Sprint(i)
		}
		if unit.returnedAt == 0 || unit.returnedAt < unit.at {
			continue
		}
		if unit.returnedAt >= f.BaselineStart && unit.returnedAt < start && unit.at < start {
			if !seenBaseline[identity] {
				baseline = append(baseline, unit.returnedAt-unit.at)
				seenBaseline[identity] = true
			}
		}
		if unit.returnedAt >= start && unit.returnedAt < limit {
			f.PeakLatency = max(f.PeakLatency, unit.returnedAt-unit.at)
			unit.identity = identity
			post = append(post, unit)
			seenPost[identity] = true
		}
	}
	f.BaselineSamples, f.PostEventSamples = len(baseline), len(seenPost)
	slices.Sort(baseline)
	if len(baseline) > 0 {
		f.BaselineMedian = baseline[len(baseline)/2]
		f.BaselineP95 = baseline[(95*len(baseline)+99)/100-1]
	}
	slices.SortFunc(post, func(a, b contentUnit) int { return cmp.Compare(a.returnedAt, b.returnedAt) })
	sustained := 0
	counted := map[string]bool{}
	for _, unit := range post {
		latency := unit.returnedAt - unit.at
		f.PeakLatency = max(f.PeakLatency, latency)
		if resumed == 0 || unit.returnedAt < resumed {
			continue
		}
		if latency > f.BaselineP95+f.Tolerance {
			sustained = 0
		} else if !counted[unit.identity] {
			sustained++
		}
		counted[unit.identity] = true
		if !f.Recovered && sustained >= f.SustainedUnits {
			f.BackToBaseline, f.Recovered = unit.returnedAt-start, true
		}
	}
	f.PeakAboveBaseline = max(0, f.PeakLatency-f.BaselineP95)
	return f
}

func freshnessReasons(kind string, f FreshnessReport) []string {
	var reasons []string
	if f.BaselineSamples < baselineMinSamples {
		reasons = append(reasons, fmt.Sprintf("%s baseline has %d samples (need %d)", kind, f.BaselineSamples, baselineMinSamples))
	}
	if f.BaselineP95-f.BaselineMedian > f.SpreadLimit {
		reasons = append(reasons, kind+" baseline p95-minus-median exceeds 20ms")
	}
	if !f.Recovered {
		reasons = append(reasons, kind+" freshness recovery not observed (need 3 sustained units)")
	}
	return reasons
}

// Summary prints an event record with explicit units and measurement policy.
func (m EventMeasurement) Summary() string {
	if m.SettleWindow == 0 {
		return "inconclusive: event measurement not available"
	}
	state := "conclusive"
	if m.Inconclusive {
		state = "inconclusive: " + strings.Join(m.Reasons, "; ")
	}
	return fmt.Sprintf("pause audio=%s video=%s; lost audio=%s video=%d frames; window=[%s,%s) settle=%s; freshness audio{%s} video{%s}; first_live=%s first_content=%s picture_id=%d sent_at=%s; %s",
		m.AudioPause, m.VideoPause, m.LostAudio, m.LostVideoFrames, m.WindowStart, m.WindowEnd, m.SettleWindow,
		m.Audio.summary(), m.Video.summary(), observedDuration(m.FirstLiveFrame, m.FirstLiveFrame > 0), m.FirstContent, m.FirstContentPictureID, m.FirstContentSentAt, state)
}

func (f FreshnessReport) summary() string {
	return fmt.Sprintf("baseline_n=%d median=%s p95=%s peak=%s above_p95=%s back=%s tolerance=%s sustained=%d spread_limit=%s baseline_start=%s", f.BaselineSamples, f.BaselineMedian, f.BaselineP95, f.PeakLatency, f.PeakAboveBaseline, observedDuration(f.BackToBaseline, f.Recovered), f.Tolerance, f.SustainedUnits, f.SpreadLimit, f.BaselineStart)
}

func observedDuration(d time.Duration, observed bool) string {
	if !observed {
		return "not observed"
	}
	return d.String()
}
