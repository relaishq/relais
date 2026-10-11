package callharness

import (
	"context"
	"github.com/pion/rtp/codecs"
	"image"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestSourceOrderedReplayDeltaDecodesAcrossMargin(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordered", true: "missing source"}[missing], func(t *testing.T) {
			r := newRecorder()
			track := r.addTrack(kindVideo, 1, 96)
			src, err := newVP8Source(callerVideo)
			require.NoError(t, err)
			for i := 0; i < 3; i++ {
				data, key, err := src.next()
				require.NoError(t, err)
				r.sentFrames[kindVideo][string(data)] = struct{}{}
				r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: time.Duration(i+1) * time.Second, data: string(data), written: true}
				if missing && i == 1 {
					continue
				}
				seq := uint16(i)
				if !missing && i > 0 {
					seq += 8192
				}
				if missing && i == 2 {
					seq = 1
				}
				frame := &vp8Frame{data: data, keyframe: key, pictureID: uint16(i), timestamp: uint32(i * 3000), firstSeq: seq, lastSeq: seq, firstArrival: r.start.Add(time.Duration(i+1) * time.Second)}
				size, err := decodeKeyframe(data)
				if !key {
					err = nil
				}
				r.videoFrame(track, frame, size, err, r.start.Add(time.Duration(i+1)*time.Second+time.Millisecond))
			}
			if missing {
				require.Equal(t, 1, track.video.DecodableFrames)
				return
			}
			require.Equal(t, 3, track.video.DecodableFrames)
			frames, size := r.decodeInput(0)
			full := fullDecode(context.Background(), frames, size)
			require.Empty(t, full.Skipped)
			require.Empty(t, full.Errors)
			require.Equal(t, 3, full.FramesDecoded)
		})
	}
}

func TestContentResumeWithoutSequenceMargin(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindAudio, 1, 111)
	for i, at := range []time.Duration{time.Second, 1020 * time.Millisecond, 1800 * time.Millisecond} {
		payload, err := r.sendingAudio([]byte{0xf8, 1}, opusFrameDuration)
		require.NoError(t, err)
		unit := r.sentAudioUnits[string(payload)]
		unit.at = at - time.Millisecond
		r.sentAudioUnits[string(payload)] = unit
		r.sent(kindAudio, false)
		r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}, Payload: payload}, r.start.Add(at))
	}
	require.Equal(t, 1800*time.Millisecond, r.videoRecovery(1500*time.Millisecond, 3*time.Second).MediaResumedAt)
}

func TestAudioIdentityStartsDifferentlyAcrossCalls(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		r := newRecorder()
		payload, err := r.sendingAudio([]byte{0xf8, 1}, opusFrameDuration)
		require.NoError(t, err)
		require.False(t, seen[string(payload)])
		seen[string(payload)] = true
	}
}
func TestLegacySendingDoesNotCreateEmptyAudioIdentity(t *testing.T) {
	r := newRecorder()
	r.sending(kindAudio, []byte{0xf8, 1})
	r.sent(kindAudio, false)
	require.Empty(t, r.sentAudioUnits)
}
func TestInvalidEarlyReceiptDoesNotHideLoss(t *testing.T) {
	r := testMeasurementRecorder(t)
	u := r.sentAudioUnits[string(rune(21))]
	u.returnedAt = u.at - time.Millisecond
	r.sentAudioUnits[string(rune(21))] = u
	require.Equal(t, 20*time.Millisecond, r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0).LostAudio)
}
func TestResumedWindowCountsLateDeliverySeparately(t *testing.T) {
	r := testMeasurementRecorder(t)
	move := testMeasurementMove()
	move.Recovery.MediaResumedAt = 2200 * time.Millisecond
	u := r.sentAudioUnits[string(rune(38))]
	u.returnedAt = 3200 * time.Millisecond
	r.sentAudioUnits[string(rune(38))] = u
	m := r.eventMeasurement(move, r.hungUpAt, 0)
	require.Equal(t, 2450*time.Millisecond, m.WindowEnd)
	require.Zero(t, m.LostAudio)
	require.Equal(t, 20*time.Millisecond, m.LateAudio)
}
func TestConclusiveCoverageRejectsUnknownRun(t *testing.T) {
	require.False(t, EnoughConclusive(0, 10))
	require.False(t, EnoughConclusive(79, 100))
	require.True(t, EnoughConclusive(80, 100))
	require.False(t, EnoughConclusive(0, 0))
}
func TestSequentialBaselineKeepsFirstAndRecoversAfterFailure(t *testing.T) {
	r := newRecorder()
	r.hungUpAt = 6 * time.Second
	r.moves = []moveRecord{{kind: "takeover", start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)}, {kind: "takeover", start: r.start.Add(4 * time.Second), end: r.start.Add(4200 * time.Millisecond)}}
	r.tracks = []*trackRecord{{kind: kindAudio, arrivals: []time.Duration{time.Second, 1800 * time.Millisecond, 3500 * time.Millisecond, 4200 * time.Millisecond}, headers: []rtpMark{{seq: 1}, {seq: 8193}, {seq: 8194}, {seq: 16386}}}}
	for i := 0; i < 240; i++ {
		at := 500*time.Millisecond + time.Duration(i)*20*time.Millisecond
		latency := time.Millisecond
		if at >= 1500*time.Millisecond {
			latency = 100 * time.Millisecond
		}
		r.sentAudioUnits[string(rune(i+1))] = contentUnit{at: at, returnedAt: at + latency, written: true, duration: 20 * time.Millisecond}
	}
	moves := r.moveReports()
	require.Len(t, moves, 2)
	require.True(t, moves[0].Measurement.Audio.Verdict.Failed)
	second := moves[1].Measurement.Audio
	require.True(t, second.Verdict.Trusted, second.Verdict.Reasons)
	require.Equal(t, time.Millisecond, second.FirstBaselineP95)
	require.Equal(t, 100*time.Millisecond, second.BaselineP95)
	require.True(t, second.LastingLag)
	require.True(t, second.Verdict.Failed)
}

func TestExplicitRequestAtTimeZeroAndAfterHangup(t *testing.T) {
	r := newRecorder()
	r.sendingVideo([]byte{1}, &keyframeResponseRequest{at: 0}, time.Second/30)
	require.True(t, r.sentVideoFrames[0].pli)
	require.Zero(t, r.sentVideoFrames[0].requestedAt)
	r.hangup()
	require.Nil(t, r.keyframeRequestReceived(kindVideo))
}
func TestLastFreshRunStartsAfterStaleTailAndFailsAfterTwoSeconds(t *testing.T) {
	var units []contentUnit
	for i := 0; i < 30; i++ {
		at := 100*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond})
	}
	for i := 0; i < 3; i++ {
		at := 1400*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at - time.Millisecond, returnedAt: at})
	}
	units = append(units, contentUnit{at: time.Second, returnedAt: 3100 * time.Millisecond})
	for i := 0; i < 3; i++ {
		at := 3200*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at - time.Millisecond, returnedAt: at})
	}
	f := measureFreshness(units, time.Second, 1400*time.Millisecond, 4*time.Second, 0)
	require.True(t, f.Recovered)
	require.Equal(t, 2200*time.Millisecond, f.BackToBaseline)
	require.Equal(t, 2100*time.Millisecond, f.LastAboveTolerance)
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed)
}

// Exercise the real encoded source, packet collector, strict frame assembler,
// source decoder chain and event judgment. Outage deltas cross a sequence
// margin without asking for a new keyframe; FFmpeg checks the actual stream.
func TestReplayedDeltaChainIsFirstNewOutageContent(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.sentVideoFrames = map[uint16]sentVideoFrame{}
	r.sentFrames[kindVideo] = map[string]struct{}{}
	r.tracks = nil
	r.sentVideo.Frames = 0
	r.frameInterval = time.Second / 30
	track := r.addTrack(kindVideo, 1, 96)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	payloader := &codecs.VP8Payloader{EnablePictureID: true}
	assembler := &vp8Assembler{}
	seq := uint16(0)
	for i := 0; i < 44; i++ {
		data, key, err := src.next()
		require.NoError(t, err)
		sent := 500*time.Millisecond + time.Duration(i)*(time.Second/30)
		arrived := sent + time.Millisecond
		if i >= 27 && i < 30 {
			arrived = 1800*time.Millisecond + time.Duration(i-27)*50*time.Millisecond
		}
		if i >= 30 {
			sent = 1910*time.Millisecond + time.Duration(i-30)*(time.Second/30)
			arrived = sent + time.Millisecond
		}
		r.sentVideoFrames[uint16(i)] = sentVideoFrame{at: sent, data: string(data), written: true, duration: time.Second / 30}
		r.sentFrames[kindVideo][string(data)] = struct{}{}
		if i == 27 {
			seq += 8192
		}
		payloads := payloader.Payload(1000, data)
		for j, payload := range payloads {
			pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: j == len(payloads)-1}, Payload: payload}
			seq++
			r.packet(track, pkt, r.start.Add(arrived))
			frame, incomplete := assembler.push(pkt)
			require.Zero(t, incomplete)
			if frame == nil {
				continue
			}
			frame.firstArrival = r.start.Add(arrived)
			frame.completedAt = r.start.Add(arrived)
			var size image.Point
			if key {
				size, err = decodeKeyframe(data)
				require.NoError(t, err)
			}
			r.videoFrame(track, frame, size, nil, r.start.Add(arrived+time.Microsecond))
		}
	}
	r.sentVideo.Frames = 44
	move := MoveReport{Kind: "takeover", Start: 1400 * time.Millisecond, End: 1700 * time.Millisecond}
	move.Recovery = r.videoRecovery(move.Start, r.hungUpAt)
	m := r.eventMeasurement(move, r.hungUpAt, 0)
	require.Equal(t, 1800*time.Millisecond, m.MediaResumedAt)
	require.True(t, m.FirstNewContentVerdict.Trusted, m.Summary())
	require.Equal(t, "outage media", m.FirstNewContent)
	require.Equal(t, uint16(27), m.FirstNewContentPictureID)
	require.Zero(t, m.LostVideoFrames)
	require.Equal(t, 44, track.video.DecodableFrames)
	frames, size := r.decodeInput(0)
	full := fullDecode(context.Background(), frames, size)
	require.Empty(t, full.Skipped)
	require.Empty(t, full.Errors)
	require.Equal(t, 44, full.FramesDecoded)
}
