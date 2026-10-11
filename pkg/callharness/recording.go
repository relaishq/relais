package callharness

import (
	"context"
	"slices"
	"sort"
	"time"
)

const (
	defaultHistory        = 2 * time.Minute
	longRunHistory        = 15 * time.Second
	maxHistoryPackets     = 32768
	maxHistoryUnits       = 16384
	maxOfflineFrames      = 4096
	maxOfflineBytes       = 16 << 20
	minimumDecodedSamples = 3
)

// RecordingOptions bounds caller history. Zero values preserve the short-call
// full-decode check, with at most two minutes of packet/content history and
// 4096 offline frames. Long calls should select DecodeEvery (e.g. 30), which
// checks samples online and retains no whole-call decode input.
// Event summaries survive history expiry. Their ObservedUntil names the finite
// observation horizon: packets older than History cannot repair a saved loss.
// History must cover the configured freshness evaluation plus baseline/margin.
type RecordingOptions struct {
	History     time.Duration
	DecodeEvery int
}

func (o RecordingOptions) defaults() RecordingOptions {
	if o.History <= 0 {
		o.History = defaultHistory
	}
	if o.DecodeEvery < 0 {
		o.DecodeEvery = 0
	}
	return o
}

// RecordingReport is a caller-owned memory gauge, separate from process heap.
// EventSummaries grow only with events, not with packets or call duration.
// HistoryBytes counts retained payload bytes; it excludes Go object overhead.
type RecordingReport struct {
	History                                                      time.Duration
	HistoryStart                                                 time.Duration
	Packets, ContentUnits, Assemblies, FrameMarks, OfflineFrames int
	EventSummaries                                               int
	HistoryBytes                                                 int64
}

func (r *recorder) memoryGauge() RecordingReport {
	g := RecordingReport{History: r.recording.History, HistoryStart: r.historyStart, ContentUnits: len(r.sentAudioUnits) + len(r.sentVideoFrames), Assemblies: len(r.contentAssemblies), EventSummaries: len(r.savedMoves)}
	for data := range r.sentAudioUnits {
		g.HistoryBytes += int64(len(data))
	}
	for _, f := range r.sentVideoFrames {
		g.HistoryBytes += int64(len(f.data))
	}
	for _, a := range r.contentAssemblies {
		for _, p := range a.parts {
			g.HistoryBytes += int64(len(p))
		}
	}
	for _, t := range r.tracks {
		g.Packets += len(t.arrivals)
		if t.video != nil {
			g.FrameMarks += len(t.video.frames)
			g.OfflineFrames += len(t.video.decodeInput)
			for _, f := range t.video.decodeInput {
				g.HistoryBytes += int64(len(f.data))
			}
		}
	}
	return g
}

// Recording snapshots retained caller history while a call is still running.
func (c *Call) Recording() RecordingReport {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	return c.rec.memoryGauge()
}

// maintainRecording discovers automatic events before their baseline expires.
// It also prunes idle calls (no media arrivals are needed to expire history).
func (c *Call) maintainRecording(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll, cancel := context.WithTimeout(ctx, time.Second)
			_ = c.collectTakeovers(poll) // Hangup still returns a failed final collection.
			cancel()
			c.rec.mu.Lock()
			if !c.rec.hungUp {
				c.rec.compact(time.Since(c.rec.start))
			}
			c.rec.mu.Unlock()
		}
	}
}

// compact runs under r.mu. Save completed event summaries before rolling away
// their baseline. A stalled event never pins history; its verdict is unknown
// when the needed window exceeds the retention bound.
func (r *recorder) compact(now time.Duration) {
	if now-r.lastCompact < time.Second {
		return
	}
	r.lastCompact = now
	for key, a := range r.contentAssemblies {
		if a.lastAt < now-time.Second {
			delete(r.contentAssemblies, key)
		}
	}
	cutoff := now - r.recording.History
	if cutoff <= 0 {
		return
	}
	oldUntil := r.hungUpAt
	r.hungUpAt = now
	reports := r.moveReports()
	r.hungUpAt = oldUntil
	count := 0
	savedCount := len(r.savedMoves)
	for i, move := range r.moves {
		if r.since(move.start)-baselineWindow >= cutoff+2*time.Second {
			break
		}
		rep := reports[savedCount+i]
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
		count++
	}
	r.moves = slices.Clone(r.moves[count:])
	for key, u := range r.sentAudioUnits {
		if u.at < cutoff {
			delete(r.sentAudioUnits, key)
		}
	}
	for key, f := range r.sentVideoFrames {
		if f.at < cutoff {
			delete(r.sentVideoFrames, key)
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
			}
		}
	}
	r.sentFrames = map[string]map[string]struct{}{kindAudio: {}, kindVideo: {}}
	for key := range r.sentAudioUnits {
		r.sentFrames[kindAudio][key] = struct{}{}
	}
	for _, f := range r.sentVideoFrames {
		r.sentFrames[kindVideo][f.data] = struct{}{}
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
		t.arrivals = slices.Clone(t.arrivals[first:])
		t.headers = slices.Clone(t.headers[first:])
		t.unmatchedTimes = slices.Clone(slices.DeleteFunc(t.unmatchedTimes, func(at time.Duration) bool { return at < cutoff }))
		t.seenPackets = make(map[uint16]uint32, len(t.headers))
		for _, mark := range t.headers {
			t.seenPackets[mark.seq] = mark.timestamp
		}
		if t.video != nil {
			t.video.frames = slices.Clone(slices.DeleteFunc(t.video.frames, func(f frameMark) bool { return f.at < cutoff }))
			t.video.sampleResults = slices.Clone(slices.DeleteFunc(t.video.sampleResults, func(s decodeSample) bool { return s.at < cutoff }))
		}
	}
	r.compactConsent(cutoff)
	first := sort.Search(len(r.decryptFailureTimes), func(i int) bool { return r.decryptFailureTimes[i] >= cutoff })
	// Saved event totals are updated by srtpError as new failures arrive.
	r.decryptFailureTimes = slices.Clone(r.decryptFailureTimes[first:])
	r.historyStart = max(r.historyStart, cutoff)
}

func (r *recorder) resetConsent(since time.Duration) {
	r.consentPrefix = ConsentReport{Since: since}
	r.consentLastResponse = since
}

func (r *recorder) compactConsent(cutoff time.Duration) {
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

func (r *recorder) appendRepeat(history []contentUnit, unit contentUnit) []contentUnit {
	if len(history) >= maxHistoryPackets {
		remove := maxHistoryPackets / 4
		r.historyStart = max(r.historyStart, history[remove].returnedAt)
		history = append(history[:0], history[remove:]...)
	}
	return append(history, unit)
}

func (r *recorder) recordUnmatched(track *trackRecord, at time.Duration) {
	if len(track.unmatchedTimes) >= maxHistoryPackets {
		remove := maxHistoryPackets / 4
		r.historyStart = max(r.historyStart, track.unmatchedTimes[remove])
		track.unmatchedTimes = slices.Delete(track.unmatchedTimes, 0, remove)
	}
	track.unmatchedTimes = append(track.unmatchedTimes, at)
}
