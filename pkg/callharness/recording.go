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
// Delivery is set only in a call's final report; live gauges leave it nil.
type RecordingReport struct {
	History                                                      time.Duration
	HistoryStart                                                 time.Duration
	Packets, ContentUnits, Assemblies, FrameMarks, OfflineFrames int
	EventSummaries                                               int
	DecodedPictures                                              int
	HistoryBytes                                                 int64
	Delivery                                                     *WholeCallDelivery `json:",omitempty"`
}

// MaxRetainedLostUnits bounds each kind's whole-call lost-unit inventory.
const MaxRetainedLostUnits = 4096

// ContentDelivery counts one kind's identified content over the whole call,
// once per successfully written audio unit or video frame. A unit settles
// when it leaves rolling history, or at hangup when it was sent at least the
// 500ms content settle window earlier; later units are Unsettled and unjudged.
// Lost uses the yardstick's rule: no valid complete return by then. Late
// returns are not lost. LostAt holds each lost unit's send time (caller clock,
// sorted), up to MaxRetainedLostUnits; further losses only add LostOverflow.
type ContentDelivery struct {
	Sent, Returned, Lost, Unsettled int
	LostAt                          []time.Duration
	LostOverflow                    int
}

// WholeCallDelivery survives rolling history as running counts, so a long
// call reports every unit without retaining every unit.
type WholeCallDelivery struct {
	Audio, Video ContentDelivery
}

// settle runs under r.mu, before a written unit leaves rolling history.
func (d *ContentDelivery) settle(u contentUnit) {
	if !u.written {
		return
	}
	d.Sent++
	if u.returnedAt > 0 && u.returnedAt >= u.at {
		d.Returned++
		return
	}
	d.Lost++
	if len(d.LostAt) < MaxRetainedLostUnits {
		d.LostAt = append(d.LostAt, u.at)
	} else {
		d.LostOverflow++
	}
}

// wholeCallDelivery runs under r.mu after hangup. Units still in history
// settle on a copy; running counts are not changed.
func (r *recorder) wholeCallDelivery() *WholeCallDelivery {
	d := &WholeCallDelivery{Audio: r.audioDelivery, Video: r.videoDelivery}
	d.Audio.LostAt = slices.Clone(r.audioDelivery.LostAt)
	d.Video.LostAt = slices.Clone(r.videoDelivery.LostAt)
	horizon := r.hungUpAt - contentSettleWindow
	remaining := func(out *ContentDelivery, u contentUnit) {
		if u.written && u.at > horizon {
			out.Unsettled++
		} else {
			out.settle(u)
		}
	}
	for _, u := range r.sentAudioUnits {
		remaining(&d.Audio, u)
	}
	for _, f := range r.sentVideoFrames {
		remaining(&d.Video, f.unit())
	}
	slices.Sort(d.Audio.LostAt)
	slices.Sort(d.Video.LostAt)
	return d
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
			g.DecodedPictures += len(t.video.decodedPictures)
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
			// Status holds the control-plane call lock while reading the store.
			// A planned move uses TryLock there, so our observer must not race it.
			// Skip a busy poll rather than delay compaction behind a long move.
			if c.harness.statusPollMu.TryLock() {
				poll, cancel := context.WithTimeout(ctx, time.Second)
				_ = c.collectTakeovers(poll) // Hangup still returns a failed final collection.
				cancel()
				c.harness.statusPollMu.Unlock()
			}
			c.rec.compactRecording(time.Since(c.rec.start))
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
	r.expireAssemblies(now, false)
	cutoff := now - r.recording.History
	if cutoff <= 0 {
		return
	}
	count := 0
	for _, move := range r.moves {
		start := move.start
		if move.event != nil {
			start = move.armedAt
		}
		if r.since(start)-baselineWindow >= cutoff+2*time.Second {
			break
		}
		count++
	}
	// eventMeasurement also uses the observation horizon for settled loss.
	// Expose this tick's horizon while measuring just the expiring prefix.
	oldUntil := r.hungUpAt
	r.hungUpAt = now
	reports := r.measureMoves(now, count)
	r.hungUpAt = oldUntil
	r.finishCompact(now, count, reports)
}

func (r *recorder) finishCompact(now time.Duration, count int, reports []MoveReport) {
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
	// Measurement views and published arrival arrays own their copies. Reuse
	// bounded history buffers here; slices.Delete clears removed pointer slots.
	r.moves = slices.Delete(r.moves, 0, count)
	if r.distributed != nil && len(r.audioSends) > 0 {
		for _, ref := range r.audioSends {
			if ref.at >= cutoff {
				break
			}
			if u, ok := r.sentAudioUnits[ref.audio]; ok && u.at == ref.at {
				r.audioDelivery.settle(u)
				delete(r.sentAudioUnits, ref.audio)
				delete(r.sentFrames[kindAudio], ref.audio)
			}
		}
	} else {
		for key, u := range r.sentAudioUnits {
			if u.at < cutoff {
				r.audioDelivery.settle(u)
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
				r.videoDelivery.settle(f.unit())
				delete(r.sentVideoFrames, ref.video)
				delete(r.sentFrames[kindVideo], f.data)
			}
		}
	} else {
		for key, f := range r.sentVideoFrames {
			if f.at < cutoff {
				r.videoDelivery.settle(f.unit())
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
				r.audioDelivery.settle(u)
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
	r.expireAssemblies(now, false)
	r.repeatedAudioReturns = slices.DeleteFunc(r.repeatedAudioReturns, func(u contentUnit) bool { return u.returnedAt < cutoff })
	r.repeatedVideoReturns = slices.DeleteFunc(r.repeatedVideoReturns, func(u contentUnit) bool { return u.returnedAt < cutoff })
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
		t.arrivals = slices.Delete(t.arrivals, 0, first)
		t.headers = slices.Delete(t.headers, 0, first)
		t.unmatchedTimes = slices.DeleteFunc(t.unmatchedTimes, func(at time.Duration) bool { return at < cutoff })
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
			t.video.frames = slices.DeleteFunc(t.video.frames, func(f frameMark) bool { return f.at < cutoff })
			t.video.sampleResults = slices.DeleteFunc(t.video.sampleResults, func(s decodeSample) bool { return s.at < cutoff })
		}
	}
	r.compactConsent(cutoff)
	first := sort.Search(len(r.decryptFailureTimes), func(i int) bool { return r.decryptFailureTimes[i] >= cutoff })
	// Saved events use counter snapshots; reporting adds subsequent failures.
	r.decryptFailureTimes = slices.Delete(r.decryptFailureTimes, 0, first)
	r.audioSends = pruneEvidenceRefs(r.audioSends, cutoff)
	r.videoSends = pruneEvidenceRefs(r.videoSends, cutoff)
	r.audioReturns = pruneEvidenceRefs(r.audioReturns, cutoff)
	r.videoReturns = pruneEvidenceRefs(r.videoReturns, cutoff)
	r.historyStart = max(r.historyStart, cutoff)
}

// expireAssemblies runs under r.mu. Assemblies idle for a second expire.
// Before the retention cutoff, distributed maintenance keeps unfinished
// fragments, because a delayed fragment can still complete them. Completed
// assemblies still expire, so they cannot fill the count limit whose eviction
// advances historyStart over evidence that time-based retention still covers.
func (r *recorder) expireAssemblies(now time.Duration, keepUnfinished bool) {
	for key, a := range r.contentAssemblies {
		if a.lastAt < now-time.Second && (!keepUnfinished || len(a.parts) == 0) {
			delete(r.contentAssemblies, key)
		}
	}
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
	r.consent = slices.Delete(r.consent, 0, first)
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
