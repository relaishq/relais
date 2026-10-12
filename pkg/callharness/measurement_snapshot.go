package callharness

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"
)

// Send and return indexes are appended on the caller hot path. Entries
// name the current identity generation, so a wrapped PictureID cannot select
// evidence belonging to a newer send. Snapshotting does no classification.
type evidenceRef struct {
	at         time.Duration
	audio      string
	video      uint16
	generation uint64
}

func refsAfter(refs []evidenceRef, lower time.Duration) []evidenceRef {
	first := sort.Search(len(refs), func(i int) bool { return refs[i].at >= lower })
	return refs[first:]
}
func pruneEvidenceRefs(refs []evidenceRef, lower time.Duration) []evidenceRef {
	return slices.Clone(refsAfter(refs, lower))
}

type unitIndex struct{ sent, returned []contentUnit }
type measurementIndex struct {
	audio, video     unitIndex
	frames           []frameMark
	margins, outages []time.Duration
}

// measurementSnapshot runs under r.mu. Copy only evidence that can contribute
// to the pending event prefix, its baselines, recovery or later first content.
// Payload strings are immutable; assembly buffers and decoder inputs are not
// needed. Saved counters are frozen here, never dereferenced during evaluation.
func (r *recorder) measurementSnapshot(until time.Duration, count int) *recorder {
	lower := until
	upper := time.Duration(0)
	for _, move := range r.moves[:count] {
		start, end, err := r.mappedMoveTimes(move)
		if err != nil {
			lower = min(lower, r.since(move.armedAt)-baselineWindow)
			continue
		}
		// Three comparison baselines plus the event's margin/settle window.
		width := max(baselineWindow, end.Upper-start.Lower+2*contentWindowMargin+contentSettleWindow)
		lower = min(lower, start.Lower-4*width)
		upper = max(upper, max(start.Upper, end.Upper)+r.freshnessPolicy.EvaluationWindow()+contentWindowMargin+contentSettleWindow)
		// A sequence-margin resume can occur long after completion. Its loss
		// window and comparison baselines must survive the snapshot too.
		for _, track := range r.tracks {
			for _, origin := range []time.Duration{start.Lower, start.Upper} {
				first := max(1, sort.Search(len(track.arrivals), func(i int) bool { return track.arrivals[i] > origin }))
				for i := first; i < len(track.arrivals) && i < len(track.headers); i++ {
					if int16(track.headers[i].seq-track.headers[i-1].seq) >= 8192-5500 {
						upper = max(upper, track.arrivals[i]+contentWindowMargin+contentSettleWindow)
						break
					}
				}
			}
		}
		// Preserve a late resume/live proof even outside the freshness window.
		for _, video := range []bool{false, true} {
			if video && r.sentVideo.Frames == 0 && r.sentVideo.SSRC == 0 {
				continue
			}
			refs := r.audioReturns
			if video {
				refs = r.videoReturns
			}
			if len(refs) == 0 {
				upper = until
				continue
			}
			anchor := end.Upper
			for range 2 {
				found := false
				for _, ref := range refsAfter(refs, anchor) {
					u := r.sentAudioUnits[ref.audio]
					if video {
						f := r.sentVideoFrames[ref.video]
						if f.identity != ref.generation {
							continue
						}
						u = f.unit()
					}
					if u.written && u.at >= anchor && u.returnedAt >= u.at && u.returnedAt <= until {
						upper = max(upper, u.returnedAt+contentWindowMargin+contentSettleWindow)
						anchor = u.returnedAt
						found = true
						break
					}
				}
				if !found {
					upper = until
					break
				}
			}
		}
		lower = min(lower, start.Lower-4*max(width, min(until, upper)-start.Lower))
	}
	lower = max(0, lower)
	s := &recorder{distributed: r.distributed, armed: maps.Clone(r.armed), recording: r.recording, historyStart: r.historyStart,
		baselineFloor: r.baselineFloor, firstAudio: r.firstAudio, firstVideo: r.firstVideo, freshnessPolicy: r.freshnessPolicy,
		start: r.start, hungUpAt: until, connectedAt: r.connectedAt, sentAudio: r.sentAudio, sentVideo: r.sentVideo,
		frameInterval: r.frameInterval, keyframeInterval: r.keyframeInterval, contentIdentityErrors: r.contentIdentityErrors,
		decryptionFailures: r.decryptionFailures, moves: slices.Clone(r.moves), savedMoves: slices.Clone(r.savedMoves),
		sentAudioUnits: make(map[string]contentUnit), sentVideoFrames: make(map[uint16]sentVideoFrame)}
	for i := range s.savedMoves {
		m := &s.savedMoves[i]
		m.Tracks = slices.Clone(m.Tracks)
		for j := range m.Tracks {
			t := &m.Tracks[j]
			if t.packetCounter != nil {
				t.PacketsAfter += t.packetCounter.packets - t.packetsAtSave
			}
			t.packetCounter = nil
		}
		if m.resumedAfterEnd > 0 {
			m.DecryptionFailuresAfterResume += r.decryptionFailures.Total() - m.decryptFailuresAtSave
		}
		m.decryptFailuresAtSave = r.decryptionFailures.Total()
	}
	// Production uses time indexes. Directly constructed test fixtures have no
	// indexes; their fallback copies the same filtered evidence once.
	if len(r.audioSends) == 0 {
		for key, u := range r.sentAudioUnits {
			if (u.at >= lower && u.at <= upper) || (u.returnedAt >= lower && u.returnedAt <= upper) {
				s.sentAudioUnits[key] = u
			}
		}
	} else {
		for _, refs := range [][]evidenceRef{r.audioSends, r.audioReturns} {
			for _, ref := range refsAfter(refs, lower) {
				if ref.at > upper {
					break
				}
				if u, ok := r.sentAudioUnits[ref.audio]; ok {
					s.sentAudioUnits[ref.audio] = u
				}
			}
		}
	}
	if len(r.videoSends) == 0 {
		for key, f := range r.sentVideoFrames {
			if (f.at >= lower && f.at <= upper) || (f.returnedAt >= lower && f.returnedAt <= upper) {
				s.sentVideoFrames[key] = f
			}
		}
	} else {
		for _, refs := range [][]evidenceRef{r.videoSends, r.videoReturns} {
			for _, ref := range refsAfter(refs, lower) {
				if ref.at > upper {
					break
				}
				if f, ok := r.sentVideoFrames[ref.video]; ok && f.identity == ref.generation {
					s.sentVideoFrames[ref.video] = f
				}
			}
		}
	}
	for _, u := range r.repeatedAudioReturns {
		if u.returnedAt >= lower {
			s.repeatedAudioReturns = append(s.repeatedAudioReturns, u)
		}
	}
	for _, u := range r.repeatedVideoReturns {
		if u.returnedAt >= lower {
			s.repeatedVideoReturns = append(s.repeatedVideoReturns, u)
		}
	}
	for _, track := range r.tracks {
		t := *track
		// Whole arrival arrays preserve caller-local pacing diagnostics and counts.
		// Cell evaluations use indexed resume/window queries below.
		first := 0
		t.arrivals = slices.Clone(t.arrivals[first:])
		t.headers = slices.Clone(t.headers[min(first, len(t.headers)):])
		t.unmatchedTimes = slices.Clone(t.unmatchedTimes[sort.Search(len(t.unmatchedTimes), func(i int) bool { return t.unmatchedTimes[i] >= lower }):])
		if t.video != nil {
			v := *t.video
			v.frames = make([]frameMark, 0, len(t.video.frames))
			for _, f := range t.video.frames {
				if f.at >= lower {
					if source, ok := r.sentVideoFrames[f.pictureID]; ok && source.at < lower && source.at == f.source.at && source.identity == f.source.identity {
						s.sentVideoFrames[f.pictureID] = source
					}
					v.frames = append(v.frames, f)
				}
			}
			v.sampleResults = make([]decodeSample, 0, len(t.video.sampleResults))
			for _, sample := range t.video.sampleResults {
				if sample.at >= lower {
					v.sampleResults = append(v.sampleResults, sample)
				}
			}
			t.video = &v
		}
		s.tracks = append(s.tracks, &t)
	}
	s.decryptFailureTimes = slices.Clone(r.decryptFailureTimes[sort.Search(len(r.decryptFailureTimes), func(i int) bool { return r.decryptFailureTimes[i] >= lower }):])
	return s
}

func (r *recorder) prepareMeasurementIndex() {
	if r.prepared != nil {
		return
	}
	p := &measurementIndex{}
	for key, u := range r.sentAudioUnits {
		if u.written {
			u.identity = key
			p.audio.sent = append(p.audio.sent, u)
		}
	}
	for key, f := range r.sentVideoFrames {
		if f.written {
			u := f.unit()
			u.identity = strconv.FormatUint(f.identity, 10) + ":" + strconv.FormatUint(uint64(key), 10)
			p.video.sent = append(p.video.sent, u)
		}
	}
	for _, index := range []*unitIndex{&p.audio, &p.video} {
		slices.SortFunc(index.sent, func(a, b contentUnit) int { return cmp.Compare(a.at, b.at) })
		for _, u := range index.sent {
			if u.returnedAt > 0 {
				index.returned = append(index.returned, u)
			}
		}
	}
	p.audio.returned = append(p.audio.returned, r.repeatedAudioReturns...)
	p.video.returned = append(p.video.returned, r.repeatedVideoReturns...)
	for _, index := range []*unitIndex{&p.audio, &p.video} {
		slices.SortFunc(index.returned, func(a, b contentUnit) int { return cmp.Compare(a.returnedAt, b.returnedAt) })
	}
	for _, t := range r.tracks {
		interval := opusFrameDuration
		if t.video != nil {
			interval = r.frameInterval
			if interval == 0 {
				interval = time.Second / 30
			}
			p.frames = append(p.frames, t.video.frames...)
		}
		for i := 1; i < len(t.arrivals); i++ {
			at := t.arrivals[i]
			if i < len(t.headers) && int16(t.headers[i].seq-t.headers[i-1].seq) >= 8192-5500 {
				p.margins = append(p.margins, at)
			}
			if at-t.arrivals[i-1] > max(50*time.Millisecond, 3*interval) {
				p.outages = append(p.outages, at)
			}
		}
	}
	slices.Sort(p.margins)
	slices.Sort(p.outages)
	slices.SortFunc(p.frames, func(a, b frameMark) int { return cmp.Compare(a.at, b.at) })
	r.prepared = p
}
func (r *recorder) indexedUnits(video, byReturn bool, lower, upper time.Duration) []contentUnit {
	if upper < lower {
		return nil
	}
	index := &r.prepared.audio
	if video {
		index = &r.prepared.video
	}
	units := index.sent
	at := func(u contentUnit) time.Duration { return u.at }
	if byReturn {
		units = index.returned
		at = func(u contentUnit) time.Duration { return u.returnedAt }
	}
	first := sort.Search(len(units), func(i int) bool { return at(units[i]) >= lower })
	last := sort.Search(len(units), func(i int) bool { return at(units[i]) > upper })
	return units[first:last]
}

// Only resume is consumed by classificationMeasurement. Compute that field
// from indexed margin/gap observations instead of rebuilding packet diagnostics.
func (r *recorder) classificationRecovery(start, limit time.Duration) VideoRecovery {
	p := r.prepared
	if p == nil {
		return r.videoRecovery(start, limit)
	}
	i := sort.Search(len(p.margins), func(i int) bool { return p.margins[i] > start })
	if i < len(p.margins) && p.margins[i] < limit {
		return VideoRecovery{MediaResumedAt: p.margins[i]}
	}
	i = sort.Search(len(p.outages), func(i int) bool { return p.outages[i] > start })
	if i == len(p.outages) || p.outages[i] >= limit {
		return VideoRecovery{}
	}
	var resumed time.Duration
	for _, video := range []bool{false, true} {
		for _, u := range r.indexedUnits(video, true, p.outages[i], limit-1) {
			if u.written && u.returnedAt >= u.at {
				if resumed == 0 || u.returnedAt < resumed {
					resumed = u.returnedAt
				}
				break
			}
		}
	}
	return VideoRecovery{MediaResumedAt: resumed}
}

// The maintenance goroutine serialises its snapshots/commits. Final reports
// take their own atomic snapshot. The recorder lock covers copying and pruning. Event
// classification never runs under the lock used by sendingAudio or packet.
func (r *recorder) compactRecording(now time.Duration) {
	r.compactRecordingBeforeEvaluation(now, nil)
}

// beforeEvaluation observes the snapshot handoff for the lock regression test.
// Production passes nil; it runs after releasing the caller's recorder lock.
func (r *recorder) compactRecordingBeforeEvaluation(now time.Duration, beforeEvaluation func()) {
	r.measurementMu.Lock()
	defer r.measurementMu.Unlock()
	r.mu.Lock()
	lockedAt := time.Now()
	if r.hungUp || now-r.lastCompact < time.Second {
		r.mu.Unlock()
		return
	}
	if r.distributed == nil {
		r.compact(now)
		r.mu.Unlock()
		return
	}
	cutoff := now - r.recording.History
	if cutoff <= 0 {
		r.lastCompact = now
		r.mu.Unlock()
		return
	}
	count := 0
	for _, m := range r.moves {
		if r.since(m.armedAt)-baselineWindow >= cutoff+2*time.Second {
			break
		}
		count++
	}
	s := r.measurementSnapshot(now, count)
	copyHeld := time.Since(lockedAt)
	r.mu.Unlock()
	if beforeEvaluation != nil {
		beforeEvaluation()
	}
	s.prepareMeasurementIndex()
	reports := s.measureMoves(now, count)
	r.mu.Lock()
	lockedAt = time.Now()
	defer func() { r.measurementLockTime = max(copyHeld, time.Since(lockedAt)); r.mu.Unlock() }()
	if r.hungUp {
		return
	}
	// New arrivals/receipts can append while evaluating; the pending prefix is
	// stable because this is the only compactor. Counters use the snapshot horizon.
	for i := range reports {
		reports[i].decryptFailuresAtSave = s.decryptionFailures.Total()
		for j := range reports[i].Tracks {
			reports[i].Tracks[j].packetCounter = r.tracks[j]
			reports[i].Tracks[j].packetsAtSave = s.tracks[j].packets
		}
	}
	r.lastCompact = now
	r.finishCompact(now, count, reports)
}
