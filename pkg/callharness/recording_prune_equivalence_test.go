package callharness

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Frozen pre-118 pruning oracle. Capacity may differ; retained evidence cannot.
func (r *recorder) legacyFinishCompact(now time.Duration, count int, reports []MoveReport) {
	cutoff := now - r.recording.History
	for _, rep := range reports {
		if r.distributed == nil {
			rep.decryptFailuresAtSave = r.decryptionFailures.Total()
		}
		for i := range rep.Tracks {
			if rep.Tracks[i].packetCounter == nil {
				rep.Tracks[i].packetCounter = r.tracks[i]
				rep.Tracks[i].packetsAtSave = r.tracks[i].packets
			}
		}
		r.savedMoves = append(r.savedMoves, rep)
		if len(r.savedMoves) == 1 {
			r.firstAudio, r.firstVideo = rep.Measurement.Audio, rep.Measurement.Video
		}
		if rep.Kind == "takeover" {
			m := rep.Measurement
			if m.Audio.Recovered && (!m.VideoExpected || m.Video.Recovered) {
				r.baselineFloor = rep.Start + max(m.Audio.BackToBaseline, m.Video.BackToBaseline)
			} else {
				r.baselineFloor = max(rep.End, m.MediaResumedAt)
			}
		}
	}
	r.moves = slices.Clone(r.moves[count:])
	if r.distributed != nil && len(r.audioSends) > 0 {
		for _, ref := range r.audioSends {
			if ref.at >= cutoff {
				break
			}
			if u, ok := r.sentAudioUnits[ref.audio]; ok && u.at == ref.at {
				delete(r.sentAudioUnits, ref.audio)
				delete(r.sentFrames[kindAudio], ref.audio)
			}
		}
	} else {
		for key, u := range r.sentAudioUnits {
			if u.at < cutoff {
				delete(r.sentAudioUnits, key)
				delete(r.sentFrames[kindAudio], key)
			}
		}
	}
	if r.distributed != nil && len(r.videoSends) > 0 {
		for _, ref := range r.videoSends {
			if ref.at >= cutoff {
				break
			}
			if f, ok := r.sentVideoFrames[ref.video]; ok && f.identity == ref.generation && f.at == ref.at {
				delete(r.sentVideoFrames, ref.video)
				delete(r.sentFrames[kindVideo], f.data)
			}
		}
	} else {
		for key, f := range r.sentVideoFrames {
			if f.at < cutoff {
				delete(r.sentVideoFrames, key)
			}
		}
	}
	// A rate spike may exhaust the count bound before the time bound. Advance
	// HistoryStart so affected events become inconclusive instead of false passes.
	if len(r.sentAudioUnits) > maxHistoryUnits {
		ats := make([]time.Duration, 0, len(r.sentAudioUnits))
		for _, u := range r.sentAudioUnits {
			ats = append(ats, u.at)
		}
		slices.Sort(ats)
		cutoff = max(cutoff, ats[len(ats)-maxHistoryUnits])
		for key, u := range r.sentAudioUnits {
			if u.at < cutoff {
				delete(r.sentAudioUnits, key)
				delete(r.sentFrames[kindAudio], key)
			}
		}
	}
	if r.distributed == nil {
		r.sentFrames = map[string]map[string]struct{}{kindAudio: {}, kindVideo: {}}
		for key := range r.sentAudioUnits {
			r.sentFrames[kindAudio][key] = struct{}{}
		}
		for _, f := range r.sentVideoFrames {
			r.sentFrames[kindVideo][f.data] = struct{}{}
		}
	}
	for key, a := range r.contentAssemblies {
		if a.lastAt < now-time.Second {
			delete(r.contentAssemblies, key)
		}
	}
	r.repeatedAudioReturns = slices.Clone(slices.DeleteFunc(r.repeatedAudioReturns, func(u contentUnit) bool { return u.returnedAt < cutoff }))
	r.repeatedVideoReturns = slices.Clone(slices.DeleteFunc(r.repeatedVideoReturns, func(u contentUnit) bool { return u.returnedAt < cutoff }))
	for _, t := range r.tracks {
		first := sort.Search(len(t.arrivals), func(i int) bool { return t.arrivals[i] >= cutoff })
		// Keep the predecessor to measure a gap that crosses the window edge.
		first = max(0, first-1)
		if len(t.arrivals)-first > maxHistoryPackets {
			first = len(t.arrivals) - maxHistoryPackets
			cutoff = max(cutoff, t.arrivals[first])
		}
		if r.distributed != nil {
			for _, mark := range t.headers[:min(first, len(t.headers))] {
				if timestamp, ok := t.seenPackets[mark.seq]; ok && timestamp == mark.timestamp {
					delete(t.seenPackets, mark.seq)
				}
			}
		}
		t.arrivals = slices.Clone(t.arrivals[first:])
		t.headers = slices.Clone(t.headers[first:])
		t.unmatchedTimes = slices.Clone(slices.DeleteFunc(t.unmatchedTimes, func(at time.Duration) bool { return at < cutoff }))
		if r.distributed == nil {
			t.seenPackets = make(map[uint16]uint32, len(t.headers))
			for _, mark := range t.headers {
				t.seenPackets[mark.seq] = mark.timestamp
			}
		}
		if t.video != nil {
			for id, picture := range t.video.decodedPictures {
				if picture.at < cutoff {
					delete(t.video.decodedPictures, id)
				}
			}
			t.video.frames = slices.Clone(slices.DeleteFunc(t.video.frames, func(f frameMark) bool { return f.at < cutoff }))
			t.video.sampleResults = slices.Clone(slices.DeleteFunc(t.video.sampleResults, func(s decodeSample) bool { return s.at < cutoff }))
		}
	}
	r.legacyCompactConsent(cutoff)
	first := sort.Search(len(r.decryptFailureTimes), func(i int) bool { return r.decryptFailureTimes[i] >= cutoff })
	// Saved events use counter snapshots; reporting adds subsequent failures.
	r.decryptFailureTimes = slices.Clone(r.decryptFailureTimes[first:])
	r.audioSends = legacyPruneEvidenceRefs(r.audioSends, cutoff)
	r.videoSends = legacyPruneEvidenceRefs(r.videoSends, cutoff)
	r.audioReturns = legacyPruneEvidenceRefs(r.audioReturns, cutoff)
	r.videoReturns = legacyPruneEvidenceRefs(r.videoReturns, cutoff)
	r.historyStart = max(r.historyStart, cutoff)
}

func (r *recorder) legacyCompactConsent(cutoff time.Duration) {
	if len(r.consent) < 2 {
		return
	}
	first := sort.Search(len(r.consent), func(i int) bool { return r.consent[i].at >= cutoff })
	first = max(0, first-1)
	p := &r.consentPrefix
	if p.Since == 0 {
		p.Since = r.connectedAt
		r.consentLastResponse = p.Since
	}
	for _, sample := range r.consent[:first] {
		if sample.at > p.Since && sample.responses > r.consentBaseResponses {
			p.ResponsesAfter += sample.responses - r.consentBaseResponses
			p.LongestWithoutResponse = max(p.LongestWithoutResponse, sample.at-r.consentLastResponse)
			r.consentLastResponse = sample.at
		}
		r.consentBaseResponses = sample.responses
	}
	r.consent = slices.Clone(r.consent[first:])
}

func legacyPruneEvidenceRefs(refs []evidenceRef, lower time.Duration) []evidenceRef {
	return slices.Clone(refsAfter(refs, lower))
}
func TestRecordingPruneEvidenceEquivalent(t *testing.T) {
	calls := costCalls(t, 0)
	old, newer := calls[0].rec, calls[1].rec
	newer.start = old.start
	newer.distributed = old.distributed
	// Fixture map iteration can order simultaneous returns differently. Both
	// implementations must start with the exact same production-index order.
	newer.audioSends = slices.Clone(old.audioSends)
	newer.audioReturns = slices.Clone(old.audioReturns)
	newer.videoSends = slices.Clone(old.videoSends)
	newer.videoReturns = slices.Clone(old.videoReturns)
	writtenAudio, writtenVideo := 0, 0
	for _, u := range newer.sentAudioUnits {
		if u.written {
			writtenAudio++
		}
	}
	for _, f := range newer.sentVideoFrames {
		if f.written {
			writtenVideo++
		}
	}
	for _, now := range []time.Duration{190 * time.Second, 191 * time.Second, 200 * time.Second} {
		old.legacyFinishCompact(now, 0, nil)
		newer.finishCompact(now, 0, nil)
		require.Equal(t, old.sentAudioUnits, newer.sentAudioUnits)
		require.Equal(t, old.sentVideoFrames, newer.sentVideoFrames)
		require.Equal(t, old.audioSends, newer.audioSends)
		require.Equal(t, old.audioReturns, newer.audioReturns)
		require.Equal(t, old.videoSends, newer.videoSends)
		require.Equal(t, old.videoReturns, newer.videoReturns)
		require.Equal(t, old.tracks, newer.tracks)
		a, b := old.report(), newer.report()
		a.ReportBuildDuration, b.ReportBuildDuration = 0, 0
		// The frozen oracle predates whole-call delivery (#134); pruning must
		// still settle every written unit it drops.
		d := b.Recording.Delivery
		require.Equal(t, writtenAudio, d.Audio.Sent+d.Audio.Unsettled)
		require.Equal(t, writtenVideo, d.Video.Sent+d.Video.Unsettled)
		a.Recording.Delivery, b.Recording.Delivery = nil, nil
		require.Equal(t, a, b)
	}
}

func TestRecordingPrunePreservesMeasurementView(t *testing.T) {
	r := costCalls(t, 2)[0].rec
	view := r.measurementSnapshot(r.hungUpAt, len(r.moves))
	before := view.moveReports()
	// A final report can be evaluating its view while maintenance prunes and
	// reuses the live recording's arrays. Its evidence must remain independent.
	r.finishCompact(200*time.Second, 0, nil)
	after := view.moveReports()
	require.Equal(t, before, after)
}
