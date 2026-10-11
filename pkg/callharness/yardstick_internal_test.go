package callharness

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/stretchr/testify/require"
)

func TestAudioIdentityRepeatsAndWriteFailure(t *testing.T) {
	r := newRecorder()
	first, err := r.sendingAudio([]byte{0xf8, 1, 2, 3}, opusFrameDuration)
	require.NoError(t, err)
	r.sent(kindAudio, false)
	second, err := r.sendingAudio([]byte{0xf8, 1, 2, 3}, opusFrameDuration)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.Equal(t, first[3:len(first)-8], second[3:len(second)-8])
	require.Equal(t, binary.BigEndian.Uint64(first[len(first)-8:])+1, binary.BigEndian.Uint64(second[len(second)-8:]))
	require.True(t, r.sentAudioUnits[string(first)].written)
	require.False(t, r.sentAudioUnits[string(second)].written, "a failed WriteSample is not sent content")
	_, err = identifyOpus([]byte{0xf9, 1}, 3)
	require.Error(t, err)
}

// Decode the whole fixture twice, including its file-loop repeat. Identity
// padding must yield identical PCM samples, not just acceptable packet framing.
func TestAudioIdentityPreservesDecodedPCM(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("RELAIS_HARNESS_REQUIRE_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg not available")
	}
	var original, marked bytes.Buffer
	writers := make([]*oggwriter.OggWriter, 2)
	writers[0], err = oggwriter.NewWith(&original, opusSampleRate, 1)
	require.NoError(t, err)
	writers[1], err = oggwriter.NewWith(&marked, opusSampleRate, 1)
	require.NoError(t, err)
	src, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	for i := 0; i < 510; i++ {
		frame, _, nextErr := src.next()
		require.NoError(t, nextErr)
		identity, identityErr := identifyOpus(frame, uint64(i+1))
		require.NoError(t, identityErr)
		for j, payload := range [][]byte{frame, identity} {
			require.NoError(t, writers[j].WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: uint32(i * 960)}, Payload: payload}))
		}
	}
	for _, writer := range writers {
		require.NoError(t, writer.Close())
	}
	decode := func(data *bytes.Buffer) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "-hide_banner", "-nostdin", "-v", "error", "-xerror", "-threads", "1", "-f", "ogg", "-i", "pipe:0", "-f", "s16le", "pipe:1")
		cmd.Stdin = data
		out, decodeErr := cmd.Output()
		require.NoError(t, decodeErr)
		require.NotEmpty(t, out)
		return out
	}
	require.Equal(t, decode(&original), decode(&marked), "per-send padding changes no decoded samples")
}

func testMeasurementRecorder(t *testing.T) *recorder {
	t.Helper()
	r := newRecorder()
	r.hungUpAt = 4 * time.Second
	r.sentVideo.Frames = 40
	for i := 0; i < 40; i++ {
		at := 500*time.Millisecond + time.Duration(i)*50*time.Millisecond
		unit := contentUnit{at: at, returnedAt: at + time.Millisecond, duration: 20 * time.Millisecond, written: true}
		r.sentAudioUnits[string(rune(i))] = unit
		r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: at, returnedAt: unit.returnedAt, duration: time.Second / 30, written: true}
	}
	frames := []frameMark{}
	for i := 0; i < 6; i++ {
		frames = append(frames, frameMark{at: 1850*time.Millisecond + time.Duration(i)*50*time.Millisecond, firstArrival: 1849*time.Millisecond + time.Duration(i)*50*time.Millisecond, decodable: true, source: sentVideoFrame{at: 1848*time.Millisecond + time.Duration(i)*50*time.Millisecond, pli: i == 0, requestedAt: 1840 * time.Millisecond}, pictureID: uint16(i)})
	}
	r.tracks = []*trackRecord{{kind: kindVideo, video: &videoRecord{frames: frames}}}
	return r
}

func testMeasurementMove() MoveReport {
	return MoveReport{Kind: "takeover", Start: 1500 * time.Millisecond, End: 1800 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 1800 * time.Millisecond}, Tracks: []MoveTrackReport{{Kind: kindAudio, Gap: 320 * time.Millisecond}, {Kind: kindVideo, Gap: 333 * time.Millisecond}}}
}

func TestEventMeasurementCountsMissingContentAndLateDelivery(t *testing.T) {
	r := testMeasurementRecorder(t)
	move := testMeasurementMove()
	// Same amount of pause, independent lost-content and freshness outcomes.
	audio := r.sentAudioUnits[string(rune(21))]
	audio.returnedAt = 0
	r.sentAudioUnits[string(rune(21))] = audio
	video := r.sentVideoFrames[22]
	video.returnedAt = 0
	r.sentVideoFrames[22] = video
	// Late outage audio/video are delivered inside the settle window.
	audio = r.sentAudioUnits[string(rune(23))]
	audio.returnedAt = 2100 * time.Millisecond
	r.sentAudioUnits[string(rune(23))] = audio
	video = r.sentVideoFrames[23]
	video.returnedAt = 2200 * time.Millisecond
	r.sentVideoFrames[23] = video
	m := r.eventMeasurement(move, r.hungUpAt, 0)
	require.False(t, m.Inconclusive, m.Reasons)
	require.Equal(t, 20*time.Millisecond, m.LostAudio)
	require.Equal(t, 1, m.LostVideoFrames)
	require.Equal(t, 450*time.Millisecond, m.Audio.PeakLatency)
	require.Equal(t, 550*time.Millisecond, m.Video.PeakLatency)
	require.Equal(t, 350*time.Millisecond, m.FirstLiveFrame)
	require.Equal(t, "keyframe", m.FirstContent)
	require.Equal(t, move.Tracks[0].Gap, m.AudioPause)
	require.Equal(t, move.Tracks[1].Gap, m.VideoPause)
	// Delivery after settle is late, independent from missing content.
	audio.returnedAt = 2600 * time.Millisecond
	r.sentAudioUnits[string(rune(23))] = audio
	late := r.eventMeasurement(move, r.hungUpAt, 0)
	require.Equal(t, 20*time.Millisecond, late.LostAudio)
	require.Equal(t, 20*time.Millisecond, late.LateAudio)
}

func TestEventMeasurementClassifiesFirstDecodedContent(t *testing.T) {
	for _, tt := range []struct {
		name    string
		sent    time.Duration
		pli     bool
		request time.Duration
		class   string
	}{
		{"cache", time.Second, false, 0, "cache"},
		{"outage", 1600 * time.Millisecond, false, 0, "outage media"},
		{"response just before resume", 1799 * time.Millisecond, true, 1750 * time.Millisecond, "keyframe"},
		{"response to pre-event request", 1700 * time.Millisecond, true, 1400 * time.Millisecond, "outage media"},
		{"cached response", time.Second, true, 900 * time.Millisecond, "cache"},
		{"requested keyframe", 1848 * time.Millisecond, true, 1820 * time.Millisecond, "keyframe"},
		{"ordinary live", 1848 * time.Millisecond, false, 0, "live"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := testMeasurementRecorder(t)
			returned := time.Duration(0)
			if tt.class == "cache" {
				returned = tt.sent + time.Millisecond
			}
			r.tracks[0].video.frames[0].source = sentVideoFrame{at: tt.sent, returnedAt: returned, pli: tt.pli, requestedAt: tt.request}
			m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
			require.Equal(t, tt.class, m.FirstContent)
			require.Positive(t, m.FirstLiveFrame)
			require.GreaterOrEqual(t, r.tracks[0].video.frames[len(r.tracks[0].video.frames)-1].source.at, m.MediaResumedAt)
		})
	}
}

func TestEventMeasurementInconclusive(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*recorder)
		reason string
	}{
		{"few samples", func(r *recorder) { r.sentAudioUnits = map[string]contentUnit{} }, "baseline has 0 samples"},
		{"noisy", func(r *recorder) {
			for key, u := range r.sentAudioUnits {
				if u.at == time.Second || u.at == 1050*time.Millisecond {
					u.returnedAt = u.at + 100*time.Millisecond
					r.sentAudioUnits[key] = u
				}
			}
		}, "baseline p95-minus-median exceeds 20ms"},
		{"short settle", func(r *recorder) { r.hungUpAt = 2300 * time.Millisecond }, "settle window truncated"},
		{"wrap", func(r *recorder) { r.sentVideo.Frames = 32769 }, "PictureID wrapped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := testMeasurementRecorder(t)
			tt.change(r)
			m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
			require.True(t, m.Inconclusive)
			require.Contains(t, m.Summary(), tt.reason)
		})
	}
}

func TestFreshnessRecoveryNeedsSustainedUniqueUnits(t *testing.T) {
	units := []contentUnit{}
	for i := 0; i < 20; i++ {
		at := time.Duration(i) * 40 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond})
	}
	for i, latency := range []time.Duration{time.Millisecond, time.Millisecond, 50 * time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond} {
		at := 1100*time.Millisecond + time.Duration(i)*100*time.Millisecond
		units = append(units, contentUnit{at: at - latency, returnedAt: at})
	}
	f := measureFreshness(units, time.Second, 1100*time.Millisecond, 2*time.Second, 0)
	require.True(t, f.Recovered)
	require.Equal(t, 200*time.Millisecond, f.BackToBaseline) // isolated live jitter does not reset recovery
	require.Equal(t, 49*time.Millisecond, f.PeakAboveBaseline)
}

// Exercise the production packet recorder: a marker arriving before the first
// fragment, cross-frame reordering, duplicated audio and video, sequence wrap,
// and exact repeated source audio must not produce false loss or extra samples.
func TestReorderedDuplicatedContentHasNoFalseLoss(t *testing.T) {
	r := newRecorder()
	audio := r.addTrack(kindAudio, 1, 111)
	video := r.addTrack(kindVideo, 2, 96)
	audioSource, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	audioFrame, _, err := audioSource.next()
	require.NoError(t, err)
	videoSource, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, keyframe, err := videoSource.next()
	require.NoError(t, err)
	require.True(t, keyframe)
	r.sentFrames[kindVideo][string(data)] = struct{}{}
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	var frames [][][]byte
	var audioPayloads [][]byte
	for i := 0; i < 15; i++ {
		at := time.Second + time.Duration(i)*40*time.Millisecond
		if i == 10 || i == 11 {
			at = 1800*time.Millisecond + time.Duration(i-10)*40*time.Millisecond
		}
		if i >= 12 {
			at = 2040*time.Millisecond + time.Duration(i-12)*40*time.Millisecond
		}
		payload, identityErr := r.sendingAudio(audioFrame, opusFrameDuration)
		require.NoError(t, identityErr)
		unit := r.sentAudioUnits[string(payload)]
		unit.at = at
		r.sentAudioUnits[string(payload)] = unit
		r.sent(kindAudio, false)
		audioPayloads = append(audioPayloads, payload)
		r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: at, data: string(data), written: true}
		frames = append(frames, payloader.Payload(1000, data))
	}
	r.sentVideo.Frames = 15
	assembler := &vp8Assembler{}
	deliverAudio := func(i int, arrived time.Duration) {
		r.packet(audio, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}, Payload: audioPayloads[i]}, r.start.Add(arrived))
	}
	deliverVideo := func(i, j int, arrived time.Duration, decode bool) {
		packet := &rtp.Packet{Header: rtp.Header{Timestamp: uint32(i * 3000), SequenceNumber: uint16(65534 + i*len(frames[i]) + j), Marker: j == len(frames[i])-1}, Payload: frames[i][j]}
		r.packet(video, packet, r.start.Add(arrived))
		if decode {
			frame, _ := assembler.push(packet)
			if frame != nil {
				frame.firstArrival = r.start.Add(arrived)
				frame.completedAt = r.start.Add(arrived)
				size, decodeErr := decodeKeyframe(frame.data)
				require.NoError(t, decodeErr)
				r.videoFrame(video, frame, size, decodeErr, r.start.Add(arrived+100*time.Microsecond))
			}
		}
	}
	// Ten real received units establish the baseline. Three decoded, fresh
	// keyframes after the reordered units establish trusted recovery.
	deliverOrdered := func(i int) {
		arrived := r.sentVideoFrames[uint16(i)].at + time.Millisecond
		deliverAudio(i, arrived)
		for j := range frames[i] {
			deliverVideo(i, j, arrived, true)
		}
	}
	for i := 0; i < 10; i++ {
		deliverOrdered(i)
	}
	// Reorder both content units and fragments across frames. Send markers
	// before starts, and duplicate packets across the sequence wrap.
	for _, i := range []int{11, 10, 11, 10} {
		deliverAudio(i, 2*time.Second)
	}
	for j := len(frames[10]) - 1; j >= 0; j-- {
		for _, i := range []int{11, 10, 11} {
			deliverVideo(i, j, 2*time.Second, false)
		}
	}
	for i := 12; i < 15; i++ {
		deliverOrdered(i)
	}
	r.hungUpAt = 3 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: 1600 * time.Millisecond, End: 1900 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 2 * time.Second}}, r.hungUpAt, 0)
	require.False(t, m.Inconclusive, m.Reasons)
	require.Zero(t, m.LostAudio)
	require.Zero(t, m.LostVideoFrames)
	require.Equal(t, 5, m.Audio.PostEventSamples, "duplicates never add freshness samples")
	require.Equal(t, 5, m.Video.PostEventSamples)
	require.Equal(t, 3, m.DecodedFrames)
}

func TestFreshnessIncludesStaleReplayWithoutDuplicateSamples(t *testing.T) {
	units := make([]contentUnit, 0, 25)
	for i := 0; i < 20; i++ {
		at := time.Duration(i) * 40 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: string(rune(i + 1))})
	}
	// An already delivered source comes back as cached media after the event.
	// Its age must be visible even though it was not lost.
	units = append(units, contentUnit{at: 400 * time.Millisecond, returnedAt: 1200 * time.Millisecond, identity: string(rune(11))})
	// Three copies of one fresh unit cannot establish sustained recovery.
	for i := 0; i < 3; i++ {
		units = append(units, contentUnit{at: 1300 * time.Millisecond, returnedAt: 1301*time.Millisecond + time.Duration(i)*time.Millisecond, identity: "fresh"})
	}
	f := measureFreshness(units, time.Second, 1200*time.Millisecond, 2*time.Second, 0)
	require.Equal(t, 800*time.Millisecond, f.PeakLatency)
	require.Equal(t, 2, f.PostEventSamples)
	require.False(t, f.Recovered)
}

func TestVideoCacheReplayIsFreshnessNotLostContent(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	data := bytes.Repeat([]byte{1}, 1400)
	r.sentVideoFrames[0] = sentVideoFrame{at: time.Second, data: string(data), written: true}
	r.sentVideo.Frames = 1
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	packets := payloader.Payload(1000, data)
	for i, payload := range packets {
		r.packet(track, &rtp.Packet{Header: rtp.Header{Timestamp: 10, SequenceNumber: uint16(i), Marker: i == len(packets)-1}, Payload: payload}, r.start.Add(1001*time.Millisecond))
	}
	for i, payload := range packets {
		r.packet(track, &rtp.Packet{Header: rtp.Header{Timestamp: 20, SequenceNumber: uint16(i + 8192), Marker: i == len(packets)-1}, Payload: payload}, r.start.Add(2*time.Second))
	}
	r.hungUpAt = 3 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: 1500 * time.Millisecond, End: 1900 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 2 * time.Second}}, r.hungUpAt, 0)
	require.Zero(t, m.LostVideoFrames)
	require.Equal(t, time.Second, m.Video.PeakLatency)
	require.Equal(t, 1, m.Video.PostEventSamples)
}

func TestStaleDuplicateInterruptsFreshnessRecovery(t *testing.T) {
	units := make([]contentUnit, 0, 25)
	for i := 0; i < 20; i++ {
		at := time.Duration(i) * 40 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: string(rune(i + 1))})
	}
	for i := 0; i < 3; i++ {
		at := 1100*time.Millisecond + time.Duration(i)*100*time.Millisecond
		units = append(units, contentUnit{at: at - time.Millisecond, returnedAt: at, identity: string(rune(i + 30))})
	}
	// A late copy of the first good unit arrives between the second and third.
	units = append(units, contentUnit{at: 1099 * time.Millisecond, returnedAt: 1250 * time.Millisecond, identity: string(rune(30))})
	f := measureFreshness(units, time.Second, 1100*time.Millisecond, 2*time.Second, 0)
	require.False(t, f.Recovered, "stale duplicates reset freshness, but cannot add units")
	require.Equal(t, 3, f.PostEventSamples)
}

func TestUnknownResumeCannotAttributeFirstContent(t *testing.T) {
	r := testMeasurementRecorder(t)
	move := testMeasurementMove()
	move.Recovery.MediaResumedAt = 0
	m := r.eventMeasurement(move, r.hungUpAt, 0)
	require.False(t, m.Inconclusive)
	require.True(t, m.ResumeVerdict.Trusted)
	require.True(t, m.ResumeVerdict.Failed)
	require.True(t, m.FirstNewContentVerdict.Failed)
	require.Empty(t, m.FirstContent)
	require.Zero(t, m.FirstLiveFrame)
	require.Zero(t, m.DecodedFrames)
	require.Contains(t, m.Summary(), "media resume not observed")
}

func TestVideoReplaySameTimestampMeasuresAge(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	data := bytes.Repeat([]byte{1}, 1400)
	r.sentVideoFrames[0] = sentVideoFrame{at: time.Second, data: string(data), written: true}
	r.sentVideo.Frames = 1
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	packets := payloader.Payload(1000, data)
	for _, delivery := range []struct {
		seq uint16
		at  time.Duration
	}{{0, 1001 * time.Millisecond}, {8192, 2 * time.Second}} {
		for i, payload := range packets {
			r.packet(track, &rtp.Packet{Header: rtp.Header{Timestamp: 10, SequenceNumber: delivery.seq + uint16(i), Marker: i == len(packets)-1}, Payload: payload}, r.start.Add(delivery.at))
		}
	}
	r.hungUpAt = 3 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: 1500 * time.Millisecond, End: 1900 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 2 * time.Second}}, r.hungUpAt, 0)
	require.Zero(t, m.LostVideoFrames)
	require.Equal(t, time.Second, m.Video.PeakLatency, "a new RTP transmission of old content exposes its stale age")
	require.Equal(t, 1, m.Video.PostEventSamples)
	require.True(t, m.Inconclusive, "this unit fixture intentionally has no sampled decode baseline")
}

func TestPartialVideoThenSameTimestampReplayIsNotLost(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	data := bytes.Repeat([]byte{1}, 1400)
	r.sentVideoFrames[0] = sentVideoFrame{at: 1400 * time.Millisecond, data: string(data), written: true}
	r.sentVideo.Frames = 1
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	packets := payloader.Payload(1000, data)
	// The old worker returned only the beginning of the frame. The new one
	// replays the whole source with a sequence margin and the same timestamp.
	r.packet(track, &rtp.Packet{Header: rtp.Header{Timestamp: 10, SequenceNumber: 1}, Payload: packets[0]}, r.start.Add(1401*time.Millisecond))
	for _, i := range []int{1, 0, 1} {
		r.packet(track, &rtp.Packet{Header: rtp.Header{Timestamp: 10, SequenceNumber: uint16(8193 + i), Marker: i == 1}, Payload: packets[i]}, r.start.Add(2*time.Second))
	}
	r.hungUpAt = 3 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: 1500 * time.Millisecond, End: 1900 * time.Millisecond, Recovery: VideoRecovery{MediaResumedAt: 2 * time.Second}}, r.hungUpAt, 0)
	require.Zero(t, m.LostVideoFrames, "a complete replay repairs partial content delivery")
	require.Equal(t, 600*time.Millisecond, m.Video.PeakLatency)
}

// A prior cache burst stays visible as that event's peak age, but is outside
// the next event's baseline once both kinds have recovered.
func TestFreshnessBaselineStartsAfterPriorRecovery(t *testing.T) {
	var units []contentUnit
	for i := 0; i < 2; i++ {
		units = append(units, contentUnit{at: 500 * time.Millisecond, returnedAt: 1100*time.Millisecond + time.Duration(i)*time.Millisecond, identity: string(rune(i + 1))})
	}
	for i := 0; i < 15; i++ {
		at := 1300*time.Millisecond + time.Duration(i)*40*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: string(rune(i + 10))})
	}
	for i := 0; i < 3; i++ {
		at := 2100*time.Millisecond + time.Duration(i)*40*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: string(rune(i + 30))})
	}
	original := measureFreshness(units, 2*time.Second, 2100*time.Millisecond, 3*time.Second, 0)
	require.Contains(t, freshnessReasons("video", original), "video baseline p95-minus-median exceeds 20ms")
	recovered := measureFreshness(units, 2*time.Second, 2100*time.Millisecond, 3*time.Second, 1250*time.Millisecond)
	require.Equal(t, 1250*time.Millisecond, recovered.BaselineStart)
	require.Equal(t, 15, recovered.BaselineSamples)
	require.Equal(t, time.Millisecond, recovered.BaselineP95)
	require.Empty(t, freshnessReasons("video", recovered))
	short := measureFreshness(units, 2*time.Second, 2100*time.Millisecond, 3*time.Second, 1700*time.Millisecond)
	require.Equal(t, 5, short.BaselineSamples)
	require.Contains(t, freshnessReasons("video", short), "video baseline has 5 samples (need 10)")
	require.Equal(t, 20*time.Millisecond, short.SpreadLimit)
}

func TestVideoResponseRetainsConsumedRequestTime(t *testing.T) {
	r := newRecorder()
	requestedAt := r.keyframeRequestReceived(kindVideo)
	require.NotNil(t, requestedAt)
	r.sendingVideo([]byte{1}, requestedAt, time.Second/30)
	response := r.sentVideoFrames[0]
	require.True(t, response.pli)
	require.Equal(t, requestedAt.at, response.requestedAt)
	// A later request cannot change the response's recorded provenance.
	_ = r.keyframeRequestReceived(kindVideo)
	require.Equal(t, requestedAt.at, r.sentVideoFrames[0].requestedAt)
	r.sent(kindVideo, true)
	r.sendingVideo([]byte{2}, nil, time.Second/30)
	require.False(t, r.sentVideoFrames[1].pli)
	require.Zero(t, r.sentVideoFrames[1].requestedAt)
}
