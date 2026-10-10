package callharness

import "time"

// VideoRecovery attributes a decoded frame using the VP8 PictureID and exact
// encoded payload observed at the caller, never worker-internal replay flags.
// A PictureID is unique over these bounded tests (less than 32768 frames).
type VideoRecovery struct {
	// SourceVideoSkippedSequenceNumbers measures the old-live to new-live
	// sequence gap excluding inserted replay packets. The first replay index
	// uses a stale snapshot, so its observed jump can be smaller than the
	// margin; the resumed source stream must still carry the full margin.
	SourceVideoSkippedSequenceNumbers int
	ReplayPackets                     int

	// CachedPictureIDs includes even incomplete replayed frames, identified
	// from received RTP payload descriptors after media resumed. This proves
	// an interrupted keyframe's packets were never replayed.
	CachedPictureIDs []uint16
	MediaResumedAt   time.Duration
	// Both common-start metrics begin at the kill, for every mode. Cached
	// output can precede control-plane completion, so completion is unsuitable.
	FirstDecodedAfterKill time.Duration
	// FirstDecodedLiveAfterKill measures a decoded frame sent at/after
	// MediaResumedAt, and is zero when no such live frame decoded.
	FirstDecodedLiveAfterKill    time.Duration
	LivePath                     string
	KeyframeInterval             time.Duration
	FirstDecodedAfterMediaResume time.Duration
	FrameInterval                time.Duration
	WithinFrameInterval          bool
	Path                         string // "Cache", "Keyframe", "Live", or empty if no decoded frame
	PictureID                    uint16
	FrameSentAt                  time.Duration
}

type sentVideoFrame struct {
	at          time.Duration
	data        string
	pli         bool
	requestedAt time.Duration
	duration    time.Duration
	written     bool
	returnedAt  time.Duration
}

func (r *recorder) sendingVideo(frame []byte, requestedAt, interval time.Duration) {
	r.sending(kindVideo, frame)
	r.mu.Lock()
	defer r.mu.Unlock()
	id := uint16(r.sentVideo.Frames & 0x7fff)
	r.sentVideoFrames[id] = sentVideoFrame{at: r.since(time.Now()), data: string(frame), pli: requestedAt > 0, requestedAt: requestedAt, duration: interval}
	r.frameInterval = interval
}

// videoRecovery runs under r.mu. A forward sequence jump identifies resumed
// media even when in-flight old-worker packets arrive just after the kill.
// Search from the kill, not the control plane's completion: the first cached
// keyframe can arrive before ResumeSession has returned to the detector.
func (r *recorder) videoRecovery(start, limit time.Duration) VideoRecovery {
	result := VideoRecovery{FrameInterval: r.frameInterval, KeyframeInterval: r.keyframeInterval}
	for _, track := range r.tracks {
		for i := 1; i < len(track.arrivals); i++ {
			at := track.arrivals[i]
			if at <= start || at >= limit {
				continue
			}
			// The snapshot may lag up to 5500 old-worker indexes. Replay
			// starts at the snapshot plus the default 8192 safety margin.
			if int16(track.headers[i].seq-track.headers[i-1].seq) < 8192-5500 {
				continue
			}
			if result.MediaResumedAt == 0 || at < result.MediaResumedAt {
				result.MediaResumedAt = at
			}
			break
		}
	}
	if result.MediaResumedAt == 0 {
		return result
	}
	for _, track := range r.tracks {
		if track.video == nil {
			continue
		}
		before := -1
		for i, at := range track.arrivals {
			if at <= start {
				before = i
			} else {
				break
			}
		}
		seen := map[uint16]bool{}
		haveLive := false
		for i, mark := range track.headers {
			at := track.arrivals[i]
			source := r.sentVideoFrames[mark.pictureID]
			if at < result.MediaResumedAt || at >= limit || !mark.havePictureID || source.at == 0 {
				continue
			}
			if !haveLive {
				if source.at < start {
					result.ReplayPackets++
				} else {
					if before >= 0 {
						result.SourceVideoSkippedSequenceNumbers = int(int16(mark.seq-track.headers[before].seq)) - 1 - result.ReplayPackets
					}
					haveLive = true
				}
			}
		}
		for i, mark := range track.headers {
			at := track.arrivals[i]
			source := r.sentVideoFrames[mark.pictureID]
			if at < result.MediaResumedAt || at >= limit || !mark.havePictureID || source.at == 0 || source.at >= start || seen[mark.pictureID] {
				continue
			}
			result.CachedPictureIDs = append(result.CachedPictureIDs, mark.pictureID)
			seen[mark.pictureID] = true
		}
	}
	for _, track := range r.tracks {
		if track.video == nil {
			continue
		}
		for _, f := range track.video.frames {
			if !f.decodable || f.at < result.MediaResumedAt || f.firstArrival < result.MediaResumedAt || f.at >= limit {
				continue
			}
			if f.source.at >= result.MediaResumedAt && result.FirstDecodedLiveAfterKill == 0 {
				result.FirstDecodedLiveAfterKill = f.at - start
				result.LivePath = "Live"
				if f.source.pli {
					result.LivePath = "Keyframe"
				}
			}
			if result.Path != "" {
				continue
			}
			result.FirstDecodedAfterKill = f.at - start
			result.FirstDecodedAfterMediaResume = f.at - result.MediaResumedAt
			result.PictureID, result.FrameSentAt = f.pictureID, f.source.at
			switch {
			case f.source.at > 0 && f.source.at < start:
				result.Path = "Cache"
			case f.source.pli && f.source.at >= start:
				result.Path = "Keyframe"
			default:
				result.Path = "Live"
			}
			result.WithinFrameInterval = result.FirstDecodedAfterMediaResume <= result.FrameInterval
		}
	}
	return result
}

func (f sentVideoFrame) unit() contentUnit {
	return contentUnit{at: f.at, duration: f.duration, written: f.written, returnedAt: f.returnedAt}
}
