package callharness

import (
	"bytes"
	"context"
	"image"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

func TestVP8SendIdentityPreservesDecodedPixels(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	require.NoError(t, err)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	var original, marked []vp8Frame
	var size image.Point
	for i := 0; i < 60; i++ {
		data, key, err := src.next()
		require.NoError(t, err)
		if key {
			size, err = decodeKeyframe(identifyVP8(data, uint64(i)))
			require.NoError(t, err)
		}
		original = append(original, vp8Frame{data: data, timestamp: uint32(i * 3000)})
		marked = append(marked, vp8Frame{data: identifyVP8(data, uint64(i)), timestamp: uint32(i * 3000)})
	}
	decode := func(frames []vp8Frame) []byte {
		var input bytes.Buffer
		writeIVF(&input, frames, size)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "-v", "error", "-xerror", "-threads", "1", "-f", "ivf", "-i", "pipe:0", "-f", "framecrc", "-")
		cmd.Stdin = &input
		output, err := cmd.Output()
		require.NoError(t, err)
		return output
	}
	require.Equal(t, decode(original), decode(marked), "the identity trailer changes no decoded frame CRC")
	require.NotEqual(t, identifyVP8(original[0].data, 0), identifyVP8(original[0].data, 32768))
}

// Cross a PictureID epoch through the production send, packet, assembler and
// event-summary paths. Short mode seeds the counter just before the boundary.
func TestRecordingWrapEventsAndBoundedHistory(t *testing.T) {
	r := newRecorder()
	r.recording.History = 15 * time.Second
	track := r.addTrack(kindVideo, 1, 96)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	assembler := &vp8Assembler{}
	seq := uint16(0)
	var oldPayloads [][]byte
	first, end := 0, 34000
	if testing.Short() {
		first, end = 32500, 33300
		r.sentVideo.Frames = first
		for i := 0; i < first; i++ {
			payloader.Payload(1000, []byte{1})
		}
	}
	for i := first; i < end; i++ {
		at := time.Second + time.Duration(i-first)*(time.Second/30)
		if i == 32768 {
			require.NoError(t, src.rewind())
		}
		data, key, err := src.next()
		require.NoError(t, err)
		if i == first {
			old := &codecs.VP8Payloader{EnablePictureID: true}
			oldPayloads = old.Payload(1000, identifyVP8(data, 0))
		}
		data = r.sendingVideo(data, nil, time.Second/30)
		id := uint16(i & 0x7fff)
		source := r.sentVideoFrames[id]
		source.at = at
		r.sentVideoFrames[id] = source
		r.sent(kindVideo, key)
		payloads := payloader.Payload(1000, data)
		// Leave the full 2s recovery + 3s stability window between events.
		if i == 32600 || i == 32800 {
			r.move(moveRecord{start: r.start.Add(at), end: r.start.Add(at + time.Millisecond)})
		}
		// An ancient first-epoch payload cannot repair or match the current send.
		if i == 32768 {
			for j, p := range oldPayloads {
				r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq + 8192 + uint16(j), Timestamp: 42, Marker: j == len(oldPayloads)-1}, Payload: p}, r.start.Add(at+time.Millisecond))
			}
			require.Zero(t, r.sentVideoFrames[0].returnedAt)
		}
		for j, p := range payloads {
			pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: j == len(payloads)-1}, Payload: p}
			seq++
			arrived := r.start.Add(at + 2*time.Millisecond)
			r.packet(track, pkt, arrived)
			frames, n := assembler.pushAll(pkt, arrived)
			require.Zero(t, n)
			for _, f := range frames {
				var size image.Point
				if f.keyframe {
					size, err = decodeKeyframe(f.data)
					require.NoError(t, err)
				}
				r.videoFrame(track, f, size, nil, arrived.Add(time.Microsecond))
			}
		}
		r.compact(at + 3*time.Millisecond)
	}
	r.hungUpAt = time.Second + time.Duration(end-first)*(time.Second/30)
	report := r.report()
	require.Len(t, report.Moves, 2)
	for _, m := range report.Moves {
		require.True(t, m.Measurement.VideoLoss.Trusted, m.Measurement.Summary())
		require.Zero(t, m.Measurement.LostVideoFrames)
		require.True(t, m.Measurement.Video.Verdict.Trusted, m.Measurement.Summary())
		require.False(t, m.Measurement.Video.Verdict.Failed, m.Measurement.Summary())
		require.Equal(t, m.Measurement.Video.StabilityWindow, m.Measurement.Video.StabilityObserved)
	}
	require.Less(t, report.Moves[0].Measurement.FirstNewContentIdentity, uint64(32768))
	require.GreaterOrEqual(t, report.Moves[1].Measurement.FirstNewContentIdentity, uint64(32768))
	require.LessOrEqual(t, report.Recording.ContentUnits, 480)
	require.LessOrEqual(t, report.Recording.FrameMarks, 480)
	require.Equal(t, end, report.SentVideo.Frames)
	require.Equal(t, end-first, report.Track(kindVideo).Video.DecodableFrames)
}

func TestVP8AssemblyReorderedDuplicateLostAndSequenceWrap(t *testing.T) {
	data := bytes.Repeat([]byte{1}, 2400)
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	now := time.Now()
	assembler := &vp8Assembler{}
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	r.sentVideo.Frames = 3
	for i := 0; i < 3; i++ {
		r.sentVideoFrames[uint16(i)] = sentVideoFrame{identity: uint64(i), at: time.Second + time.Duration(i)*time.Millisecond, data: string(data), written: true}
	}
	r.sentFrames[kindVideo][string(data)] = struct{}{}
	var got []*vp8Frame
	incomplete := 0
	deliver := func(i int, order []int, at time.Time) {
		payloads := payloader.Payload(1000, data)
		for _, j := range order {
			pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(65534 + i*3 + j), Timestamp: uint32(i * 3000), Marker: j == 2}, Payload: payloads[j]}
			r.packet(track, pkt, r.start.Add(time.Second+at.Sub(now)))
			frames, n := assembler.pushAll(pkt, at)
			got = append(got, frames...)
			incomplete += n
		}
	}
	deliver(0, []int{2, 2, 0, 1, 1}, now)
	deliver(1, []int{2, 0}, now.Add(time.Millisecond)) // missing the middle fragment
	deliver(2, []int{2, 0, 1, 2}, now.Add(2*time.Millisecond))
	frames, n := assembler.pushAll(nil, now.Add(frameReorderWindow+3*time.Millisecond))
	got = append(got, frames...)
	incomplete += n
	require.Len(t, got, 2)
	require.Equal(t, data, got[0].data)
	require.Equal(t, data, got[1].data)
	require.Equal(t, uint16(0), got[0].pictureID)
	require.Equal(t, uint16(2), got[1].pictureID)
	require.Equal(t, 1, incomplete)
	for _, f := range got {
		f.firstArrival = r.start.Add(time.Second)
		f.completedAt = r.start.Add(1100 * time.Millisecond)
		r.videoFrame(track, f, image.Point{}, nil, f.completedAt)
	}
	require.Equal(t, 1, track.video.FrameGaps)
	require.Equal(t, 3, track.duplicatePackets)
	require.Positive(t, track.outOfOrderPackets)
	r.hungUpAt = 3 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: time.Second, End: 1005 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 1006 * time.Millisecond}}, r.hungUpAt, 0)
	require.True(t, m.VideoLoss.Trusted, m.Summary())
	require.Equal(t, 1, m.LostVideoFrames)
}

func TestOnlineDecodeUndersamplingIsInconclusive(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.recording.DecodeEvery = 30
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.True(t, m.Inconclusive)
	require.False(t, m.FirstNewContentVerdict.Trusted)
	require.Contains(t, m.FirstNewContentVerdict.Reasons, "too few online decoded samples")
	require.True(t, m.VideoLoss.Trusted, "content completeness does not depend on decoding")
	r.tracks[0].video.sampleResults = []decodeSample{{at: 2 * time.Second, decoded: true, sourceAt: 1999 * time.Millisecond}, {at: 2050 * time.Millisecond, decoded: true, sourceAt: 2049 * time.Millisecond}, {at: 2100 * time.Millisecond, decoded: true, sourceAt: 2099 * time.Millisecond}}
	m = r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.True(t, m.FirstNewContentVerdict.Trusted, m.Summary())
	for i := range r.tracks[0].video.sampleResults {
		r.tracks[0].video.sampleResults[i].sourceAt = 1999 * time.Millisecond
	}
	m = r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, 1, m.DecodedSamples)
	require.False(t, m.FirstNewContentVerdict.Trusted, "copies of one source cannot satisfy decode coverage")
}

func TestOnlineDecodeCoverageWithDroppedSamples(t *testing.T) {
	// Captured from the completed 30-minute run: one missed target cannot
	// erase 3,746 unique successful decodes. The drop remains in the report.
	d := OnlineDecodeReport{Every: 30, Sampled: 3747, Decoded: 3746, UniqueDecoded: 3746, Dropped: 1}
	require.True(t, d.Conclusive())
	require.Equal(t, 1, d.Dropped)
	for _, d := range []OnlineDecodeReport{
		{Sampled: 3, Decoded: 2, UniqueDecoded: 2, Dropped: 1},
		{Sampled: 100, Decoded: 79, UniqueDecoded: 79, Dropped: 21},
		{Sampled: 100, Decoded: 100, UniqueDecoded: 3},
		{Sampled: 100, Decoded: 99, UniqueDecoded: 99, Errors: 1},
	} {
		require.False(t, d.Conclusive(), "%+v", d)
	}
	require.True(t, (OnlineDecodeReport{Sampled: 100, Decoded: 80, UniqueDecoded: 80, Dropped: 20}).Conclusive())
	r := testMeasurementRecorder(t)
	r.recording.DecodeEvery = 30
	r.tracks[0].video.sampleResults = []decodeSample{
		{at: 2 * time.Second, decoded: true, sourceAt: 1999 * time.Millisecond},
		{at: 2050 * time.Millisecond, decoded: true, sourceAt: 2049 * time.Millisecond},
		{at: 2100 * time.Millisecond, decoded: true, sourceAt: 2099 * time.Millisecond},
		{at: 2150 * time.Millisecond, decoded: false, sourceAt: 2149 * time.Millisecond},
	}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.False(t, m.FirstNewContentVerdict.Trusted, "an event-local drop still makes decode attribution inconclusive")
	require.True(t, m.VideoLoss.Trusted)
}

func TestOnlineDecodeBusyWorkerKeepsRecoveryBurst(t *testing.T) {
	r := newRecorder()
	r.recording.DecodeEvery = 30
	track := r.addTrack(kindVideo, 1, 96)
	track.video.sampleBurst = minimumDecodedSamples
	s := &onlineSampler{
		call: &Call{rec: r}, track: track,
		queue: make(chan sampleJob, onlineDecodeQueueCapacity),
		gop:   []vp8Frame{{data: []byte{0}, keyframe: true}},
		size:  image.Point{X: 1, Y: 1}, bytes: 1,
	}
	// A routine target is waiting while the decoder is busy. The RTP reader
	// then completes all three recovery targets before the worker can drain.
	s.queue <- sampleJob{}
	for i := 1; i <= minimumDecodedSamples; i++ {
		track.video.Frames = i
		at := time.Second + time.Duration(i)*time.Millisecond
		data := []byte{byte(i)}
		r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: at - time.Millisecond, data: string(data)}
		s.frame(&vp8Frame{pictureID: uint16(i), data: data, completedAt: r.start.Add(at)}, image.Point{}, nil)
	}
	require.Zero(t, track.video.OnlineDecode.Dropped)
	require.Len(t, s.queue, 1+minimumDecodedSamples)
	// Capacity remains a hard bound; another routine target is still dropped
	// explicitly instead of blocking the media reader or growing the queue.
	track.video.Frames = 30
	s.frame(&vp8Frame{data: []byte{4}, completedAt: r.start.Add(2 * time.Second)}, image.Point{}, nil)
	require.Equal(t, 1, track.video.OnlineDecode.Dropped)
	require.Len(t, s.queue, onlineDecodeQueueCapacity)
}

func TestRecordingConsentPrefixKeepsTotalsAndSilentGaps(t *testing.T) {
	r := newRecorder()
	r.recording.History = 15 * time.Second
	r.connectedAt = time.Millisecond
	for i := 1; i <= 100; i++ {
		at := time.Duration(i) * time.Second
		r.consentRequest(r.start.Add(at))
		if i != 8 && i != 9 {
			r.consentResponse(r.start.Add(at + time.Millisecond))
		}
		r.compact(at + 2*time.Millisecond)
	}
	r.hungUpAt = 101 * time.Second
	rep := r.consentReport()
	require.EqualValues(t, 100, rep.RequestsSent)
	require.EqualValues(t, 98, rep.ResponsesReceived)
	require.EqualValues(t, 98, rep.ResponsesAfter)
	require.Equal(t, 3*time.Second, rep.LongestWithoutResponse)
	require.LessOrEqual(t, len(r.consent), 34)
}

func TestVP8AssemblyWholeFrameReordering(t *testing.T) {
	data := bytes.Repeat([]byte{1}, 2400)
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	payloads := [][][]byte{payloader.Payload(1000, data), payloader.Payload(1000, data), payloader.Payload(1000, data)}
	a := &vp8Assembler{}
	now := time.Now()
	var got []*vp8Frame
	deliver := func(i int, at time.Time) {
		for j, p := range payloads[i] {
			frames, n := a.pushAll(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i*3 + j), Timestamp: uint32(i * 3000), Marker: j == 2}, Payload: p}, at)
			require.Zero(t, n)
			got = append(got, frames...)
		}
	}
	deliver(0, now)
	deliver(2, now.Add(time.Millisecond))
	require.Len(t, got, 1, "a forward source gap waits for the reordered predecessor")
	deliver(1, now.Add(20*time.Millisecond))
	require.Len(t, got, 3)
	for i, f := range got {
		require.Equal(t, uint16(i), f.pictureID)
	}
}

func TestVP8AssemblyResumeGapIsNotReorderDelay(t *testing.T) {
	data := bytes.Repeat([]byte{1}, 2400)
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	a := &vp8Assembler{}
	now := time.Now()
	var got []*vp8Frame
	for i := 0; i <= 12; i++ {
		payloads := payloader.Payload(1000, data)
		if i != 0 && i != 12 {
			continue
		}
		for j, p := range payloads {
			frames, n := a.pushAll(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i*3 + j), Timestamp: uint32(i * 3000), Marker: j == 2}, Payload: p}, now.Add(time.Duration(i)*time.Millisecond))
			require.Zero(t, n)
			got = append(got, frames...)
		}
	}
	require.Len(t, got, 2, "old cached content followed by live media must not wait for the outage's source gap")
}

func TestExpiredEventFreshnessStaysInconclusiveInReport(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.historyStart = time.Second
	r.move(moveRecord{start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)})
	rep := r.report()
	require.Len(t, rep.Moves, 1)
	m := rep.Moves[0].Measurement
	require.GreaterOrEqual(t, m.Audio.BaselineSamples, baselineMinSamples)
	require.GreaterOrEqual(t, m.Video.BaselineSamples, baselineMinSamples)
	require.False(t, m.Audio.Verdict.Trusted)
	require.False(t, m.Video.Verdict.Trusted)
	require.Contains(t, m.Video.Verdict.Reasons, "event history expired before observation")
}

func TestUnknownDecodedFrameMakesFirstContentInconclusive(t *testing.T) {
	r := testMeasurementRecorder(t)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	size, err := decodeKeyframe(data)
	require.NoError(t, err)
	at := r.start.Add(1805 * time.Millisecond)
	r.videoFrame(r.tracks[0], &vp8Frame{data: data, keyframe: true, pictureID: 1001, timestamp: 3000, firstSeq: 1, lastSeq: 1, firstArrival: at, completedAt: at}, size, nil, at.Add(time.Microsecond))
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, "unknown", m.FirstContent)
	require.False(t, m.FirstContentVerdict.Trusted)
	require.False(t, m.FirstNewContentVerdict.Trusted)
	require.False(t, m.FirstLiveVerdict.Trusted)
	require.False(t, m.Video.Verdict.Trusted)
	require.True(t, m.VideoLoss.Trusted, "known successful sends still have independently measured completeness")
}

func TestUnknownReturnedAudioDoesNotPassFreshness(t *testing.T) {
	r := testMeasurementRecorder(t)
	audio := r.addTrack(kindAudio, 1, 111)
	r.packet(audio, &rtp.Packet{Payload: []byte{0xf8, 1, 99}}, r.start.Add(1805*time.Millisecond))
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.False(t, m.Audio.Verdict.Trusted)
	require.True(t, m.Video.Verdict.Trusted, "unknown audio must not invalidate the independent video metric")
	require.True(t, m.AudioLoss.Trusted)
}

func TestRecordingIdentityConflictOverridesExpiredHistoryAndUndersampling(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.historyStart = time.Second
	r.recording.DecodeEvery = 30
	r.contentIdentityErrors = 1
	r.move(moveRecord{start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)})
	m := r.report().Moves[0].Measurement
	require.False(t, m.AudioLoss.Trusted, "expired loss history remains unknown")
	for _, v := range []MetricVerdict{m.VideoLoss, m.Video.Verdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
		require.True(t, v.Trusted)
		require.True(t, v.Failed, "known corruption cannot hide in missing history or sample coverage")
		require.Contains(t, v.Reasons, "conflicting video content identities")
	}
}

func TestRecordingAbsoluteLagOverridesExpiredHistory(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.historyStart = time.Second
	r.savedMoves = []MoveReport{{Start: time.Millisecond}}
	r.firstAudio = FreshnessReport{BaselineSamples: 20, BaselineMedian: 300 * time.Millisecond, BaselineP95: 300 * time.Millisecond, SpreadLimit: baselineSpreadLimit}
	r.firstVideo = FreshnessReport{BaselineSamples: 20, BaselineMedian: time.Millisecond, BaselineP95: time.Millisecond, SpreadLimit: baselineSpreadLimit}
	r.move(moveRecord{start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)})
	m := r.report().Moves[1].Measurement
	require.True(t, m.Audio.AbsoluteLag)
	require.True(t, m.Audio.Verdict.Trusted)
	require.True(t, m.Audio.Verdict.Failed)
	require.False(t, m.Video.Verdict.Trusted, "independent expired video evidence stays unknown")
}

func TestRecordingOldVideoEpochIsUnknownNotConflicting(t *testing.T) {
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	old := identifyVP8(data, 0)
	_, err = decodeKeyframe(old)
	require.NoError(t, err)
	for _, sameIdentity := range []bool{false, true} {
		r := newRecorder()
		track := r.addTrack(kindVideo, 1, 96)
		r.sentVideoFrames[0] = sentVideoFrame{identity: 32768, at: 2 * time.Second, data: string(identifyVP8(data, 32768)), written: true}
		returned := old
		if sameIdentity {
			// A different payload claiming the current full identity is still
			// corruption, regardless of whether it could decode.
			changed := bytes.Clone(data)
			changed[0] ^= 1
			returned = identifyVP8(changed, 32768)
		}
		at := r.start.Add(3 * time.Second)
		size, decodeErr := decodeKeyframe(returned)
		r.videoFrame(track, &vp8Frame{data: returned, keyframe: true, pictureID: 0, firstArrival: at, completedAt: at}, size, decodeErr, at)
		if sameIdentity {
			require.Equal(t, 1, r.contentIdentityErrors)
		} else {
			require.Zero(t, r.contentIdentityErrors, "an expired full identity does not conflict with the current epoch")
			require.Zero(t, track.video.frames[0].source.at)
			require.Equal(t, []time.Duration{3 * time.Second}, track.unmatchedTimes)
		}
		require.Zero(t, r.sentVideoFrames[0].returnedAt, "neither payload can repair the current send")
	}
}

func TestRecordingSavedSummaryKeepsFiniteFreshnessWindow(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.recording.History = 10 * time.Second
	r.move(moveRecord{start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)})
	r.compact(13 * time.Second)
	require.Empty(t, r.moves)
	require.Len(t, r.savedMoves, 1)
	saved := r.savedMoves[0]
	require.Equal(t, 13*time.Second, saved.Measurement.ObservedUntil)
	for _, f := range []FreshnessReport{saved.Measurement.Audio, saved.Measurement.Video} {
		require.Equal(t, 6500*time.Millisecond, f.EvaluationEnd)
		require.Equal(t, 3*time.Second, f.StabilityObserved)
	}
	r.compact(time.Hour)
	require.Equal(t, saved.Measurement, r.savedMoves[0].Measurement)
	require.Zero(t, r.memoryGauge().ContentUnits)
	require.Zero(t, r.memoryGauge().FrameMarks)
}
