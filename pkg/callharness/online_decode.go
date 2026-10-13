package callharness

import (
	"context"
	"image"
	"slices"
	"time"
)

// Reserve the three recovery targets plus one routine target while a decoder
// job is busy. This stays fixed; overflow remains explicit and nonblocking.
const onlineDecodeQueueCapacity = minimumDecodedSamples + 1

// OnlineDecodeReport counts checked target frames, not the reference frames
// ffmpeg must decode before an interframe. Keyframes are always decoded in Go.
// A full queue drops a sample explicitly; it never blocks the RTP reader.
type OnlineDecodeReport struct {
	Every                                                         int
	Sampled, Decoded, UniqueDecoded, Errors, Dropped, Unavailable int
	LastError                                                     string
}

func (d OnlineDecodeReport) Conclusive() bool {
	// One dropped sample over hours does not erase the remaining coverage.
	// Reuse the yardstick's coverage gate; event-local drops still invalidate
	// the affected first-content verdicts in eventDecodeFailure.
	return d.UniqueDecoded >= minimumDecodedSamples && d.Errors == 0 && EnoughConclusive(d.UniqueDecoded, d.Sampled)
}
func (d OnlineDecodeReport) SampleRate(frames int) float64 {
	if frames == 0 {
		return 0
	}
	return float64(d.Sampled) / float64(frames)
}

type decodeSample struct {
	at       time.Duration
	decoded  bool
	sourceAt time.Duration
	failure  DecodeFailure
}
type sampleJob struct {
	frames   []vp8Frame
	size     image.Point
	at       time.Duration
	sourceAt time.Duration
}
type onlineSampler struct {
	call  *Call
	track *trackRecord
	queue chan sampleJob
	gop   []vp8Frame
	size  image.Point
	bytes int
}

func (c *Call) newOnlineSampler(track *trackRecord) *onlineSampler {
	s := &onlineSampler{call: c, track: track, queue: make(chan sampleJob, onlineDecodeQueueCapacity)}
	c.rec.mu.Lock()
	track.video.OnlineDecode.Every = c.rec.recording.DecodeEvery
	c.rec.mu.Unlock()
	c.startReader(func() {
		for job := range s.queue {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			result := fullDecode(ctx, job.frames, job.size)
			cancel()
			c.rec.mu.Lock()
			c.rec.recordOnlineDecode(track, job, result)
			c.rec.mu.Unlock()
		}
	})
	return s
}

func (s *onlineSampler) frame(f *vp8Frame, size image.Point, err error) {
	r := s.call.rec
	if f.keyframe {
		s.gop = nil
		s.bytes = 0
		s.size = size
	}
	// Reference history is capped even if an encoder never supplies a keyframe.
	if (len(s.gop) > 0 || (f.keyframe && err == nil)) && len(s.gop) < maxAssemblyPackets && s.bytes+len(f.data) <= 4*maxAssemblyBytes {
		s.gop = append(s.gop, *f)
		s.bytes += len(f.data)
	} else {
		s.gop = nil
		s.bytes = 0
	}
	r.mu.Lock()
	at := r.since(f.completedAt)
	v := s.track.video
	sourceAt := time.Duration(0)
	if source := r.sentVideoFrames[f.pictureID]; source.data == string(f.data) {
		sourceAt = source.at
	}
	if v.Frames > 1 && len(v.frames) > 1 {
		previous := v.frames[len(v.frames)-2]
		if at-previous.receivedAt > max(50*time.Millisecond, 3*r.frameInterval) {
			v.sampleBurst = 3
		}
	}
	for _, move := range r.moves {
		start := r.since(move.start)
		if move.event != nil {
			mapped, _, err := r.mappedMoveTimes(move)
			if err != nil {
				continue
			}
			start = mapped.Lower
		}
		if at >= start && start > v.lastSampleEvent {
			v.sampleBurst = max(v.sampleBurst, 3)
			v.lastSampleEvent = start
		}
	}
	wanted := f.keyframe || (v.Frames-v.RepeatedFrames)%r.recording.DecodeEvery == 0 || v.sampleBurst > 0
	if wanted && v.sampleBurst > 0 {
		v.sampleBurst--
	}
	if !wanted {
		r.mu.Unlock()
		return
	}
	d := &v.OnlineDecode
	d.Sampled++
	if f.keyframe {
		success := err == nil
		var failure DecodeFailure
		if success {
			d.Decoded++
		} else {
			d.Errors++
			d.LastError = err.Error()
			failure = DecodeInvalid
		}
		r.recordDecodeSample(s.track, decodeSample{at: at, decoded: success, sourceAt: sourceAt, failure: failure})
		r.mu.Unlock()
		return
	}
	if len(s.gop) == 0 {
		v.sampleResults = append(v.sampleResults, decodeSample{at: at, sourceAt: sourceAt, failure: DecodeUnavailable})
		d.Dropped++
		r.mu.Unlock()
		return
	}
	job := sampleJob{frames: slices.Clone(s.gop), size: s.size, at: at, sourceAt: sourceAt}
	select {
	case s.queue <- job:
	default:
		v.sampleResults = append(v.sampleResults, decodeSample{at: at, sourceAt: sourceAt, failure: DecodeUnavailable})
		d.Dropped++
	}
	r.mu.Unlock()
}

func (r *recorder) eventDecodedSamples(start, limit time.Duration) int {
	seen := map[time.Duration]bool{}
	for _, t := range r.tracks {
		if t.video != nil {
			for _, s := range t.video.sampleResults {
				if s.decoded && s.at >= start && s.at < limit && s.sourceAt > 0 {
					seen[s.sourceAt] = true
				}
			}
		}
	}
	return len(seen)
}

func (r *recorder) eventDecodeFailure(start, limit time.Duration) DecodeFailure {
	var failure DecodeFailure
	for _, t := range r.tracks {
		if t.video != nil {
			for _, s := range t.video.sampleResults {
				if !s.decoded && s.at >= start && s.at < limit {
					if s.failure == DecodeInvalid {
						return DecodeInvalid
					}
					failure = DecodeUnavailable
				}
			}
		}
	}
	return failure
}

// recordOnlineDecode runs under r.mu, after the decoder finishes outside it.
func (r *recorder) recordOnlineDecode(track *trackRecord, job sampleJob, result FullDecode) {
	d := &track.video.OnlineDecode
	success := result.Ran() && result.Errors == "" && result.FramesDecoded == len(job.frames) && result.Failure == ""
	failure := result.Failure
	if success {
		d.Decoded++
	} else {
		if failure == DecodeInvalid {
			d.Errors++
		} else {
			failure = DecodeUnavailable
			d.Unavailable++
		}
		d.LastError = result.Errors
		if result.Skipped != "" {
			d.LastError = result.Skipped
		}
	}
	r.recordDecodeSample(track, decodeSample{at: job.at, decoded: success, sourceAt: job.sourceAt, failure: failure})
}

func (r *recorder) recordDecodeSample(track *trackRecord, sample decodeSample) {
	v := track.video
	if sample.decoded && sample.sourceAt > 0 {
		seen := false
		for _, previous := range v.sampleResults {
			if previous.decoded && previous.sourceAt == sample.sourceAt {
				seen = true
				break
			}
		}
		if !seen {
			v.OnlineDecode.UniqueDecoded++
		}
	}
	v.sampleResults = append(v.sampleResults, sample)
}
