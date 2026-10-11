package callharness

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strconv"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

// Q1: a new sequence span is an independent replay, including when its
// marker arrives first or the interrupted copy has not yet expired.
func TestVP8AssemblyPartialThenSameIDReplay(t *testing.T) {
	for _, delay := range []time.Duration{20 * time.Millisecond, 400 * time.Millisecond} {
		for _, base := range []uint16{102, 8292} {
			for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}} {
				t.Run(delay.String()+"/"+strconv.Itoa(int(base))+"/"+strconv.Itoa(order[0]), func(t *testing.T) {
					data := bytes.Repeat([]byte{1}, 2500)
					p := &codecs.VP8Payloader{EnablePictureID: true}
					p.Payload(1000, []byte{1})
					packets := p.Payload(1000, data)
					require.Len(t, packets, 3)
					a := &vp8Assembler{}
					t0 := time.Now()
					var got []*vp8Frame
					incomplete := 0
					push := func(seq uint16, i int, at time.Time) {
						f, n := a.pushAll(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: 3000, Marker: i == len(packets)-1}, Payload: packets[i]}, at)
						got = append(got, f...)
						incomplete += n
					}
					push(100, 0, t0)
					push(101, 1, t0.Add(time.Millisecond))
					for j, i := range order {
						push(base+uint16(i), i, t0.Add(delay+time.Duration(j)*time.Millisecond))
					}
					f, n := a.pushAll(nil, t0.Add(time.Second))
					got = append(got, f...)
					incomplete += n
					t.Logf("Q1 emitted=%d incomplete=%d", len(got), incomplete)
					require.Len(t, got, 1)
					require.Equal(t, data, got[0].data)
					require.Equal(t, base, got[0].firstSeq)
					require.Equal(t, t0.Add(delay), got[0].firstArrival)
					require.Equal(t, 1, incomplete)
				})
			}
		}
	}
}

func TestVP8AssemblyDuplicateWhileReadyExpires(t *testing.T) {
	p := &codecs.VP8Payloader{EnablePictureID: true}
	a := &vp8Assembler{}
	start := time.Now()
	var got []*vp8Frame
	incomplete := 0
	var duplicate *rtp.Packet
	for i := 0; i < 3; i++ {
		payloads := p.Payload(1000, bytes.Repeat([]byte{1}, 2500))
		if i == 1 {
			continue // hold frame 2 briefly for its reordered predecessor
		}
		for j, payload := range payloads {
			packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i*3 + j), Timestamp: uint32(i * 3000), Marker: j == len(payloads)-1}, Payload: payload}
			frames, n := a.pushAll(packet, start.Add(time.Duration(i)*time.Millisecond))
			got = append(got, frames...)
			incomplete += n
			duplicate = packet
		}
	}
	frames, n := a.pushAll(duplicate, start.Add(102*time.Millisecond))
	got = append(got, frames...)
	incomplete += n
	frames, n = a.pushAll(nil, start.Add(time.Second))
	got = append(got, frames...)
	incomplete += n
	require.Len(t, got, 2)
	require.Zero(t, incomplete, "a duplicate of a ready frame cannot create another partial assembly")
}

// Decoder-path twin of TestPartialVideoThenSameTimestampReplayIsNotLost.
// The interrupted interframe must preserve the source chain, and appear as
// outage media rather than disappear until a later keyframe.
func TestPartialVideoThenSameTimestampReplayDecodes(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	p := &codecs.VP8Payloader{EnablePictureID: true}
	a := &vp8Assembler{}
	seq := uint16(0)
	incomplete := 0
	deliver := func(pkt *rtp.Packet, at time.Duration) {
		arrived := r.start.Add(at)
		r.packet(track, pkt, arrived)
		frames, n := a.pushAll(pkt, arrived)
		incomplete += n
		for _, f := range frames {
			var size image.Point
			var decodeErr error
			if f.keyframe {
				size, decodeErr = decodeKeyframe(f.data)
				require.NoError(t, decodeErr)
			}
			r.videoFrame(track, f, size, decodeErr, arrived.Add(time.Microsecond))
		}
	}
	for i := 0; i < 35; i++ {
		at := 500*time.Millisecond + time.Duration(i)*(time.Second/30)
		data, key, err := src.next()
		require.NoError(t, err)
		data = r.sendingVideo(data, nil, time.Second/30)
		source := r.sentVideoFrames[uint16(i)]
		source.at = at
		r.sentVideoFrames[uint16(i)] = source
		r.sent(kindVideo, key)
		packets := p.Payload(100, data)
		if i == 33 {
			require.False(t, key)
			require.Greater(t, len(packets), 2)
			for j := 0; j < 2; j++ {
				deliver(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq + uint16(j), Timestamp: uint32(i * 3000)}, Payload: packets[j]}, at+time.Millisecond)
			}
			seq += 8192
		}
		for j, payload := range packets {
			arrival := at + time.Millisecond
			if i >= 33 {
				arrival = 2050*time.Millisecond + time.Duration(i-33)*50*time.Millisecond
			}
			deliver(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: j == len(packets)-1}, Payload: payload}, arrival)
			seq++
		}
	}
	r.hungUpAt = 7 * time.Second
	m := r.eventMeasurement(MoveReport{Kind: "takeover", Start: 1650 * time.Millisecond, End: 2 * time.Second, Recovery: VideoRecovery{MediaResumedAt: 2050 * time.Millisecond}}, r.hungUpAt, 0)
	require.Equal(t, 1, incomplete)
	require.Equal(t, 35, track.video.DecodableFrames)
	require.Zero(t, track.video.FrameGaps)
	require.Zero(t, m.LostVideoFrames)
	require.Equal(t, "outage media", m.FirstNewContent)
	require.Equal(t, uint64(33), m.FirstNewContentIdentity)
}

// Q2: use the production keyframe sampler to record a real decode error,
// rather than inferring corruption from missing sample coverage.
func TestOnlineDecodeBitstreamErrorFailsEvent(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.recording.DecodeEvery = 30
	track := r.tracks[0]
	s := &onlineSampler{call: &Call{rec: r}, track: track}
	s.frame(&vp8Frame{keyframe: true, completedAt: r.start.Add(1850 * time.Millisecond)}, image.Point{}, errors.New("invalid VP8 bitstream"))
	track.video.sampleResults = append(track.video.sampleResults,
		decodeSample{at: 2 * time.Second, decoded: true, sourceAt: 1999 * time.Millisecond},
		decodeSample{at: 2050 * time.Millisecond, decoded: true, sourceAt: 2049 * time.Millisecond},
		decodeSample{at: 2100 * time.Millisecond, decoded: true, sourceAt: 2099 * time.Millisecond})
	for _, expired := range []bool{false, true} {
		if expired {
			r.historyStart = time.Second
		}
		m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
		t.Logf("Q2 expired=%t first_new=%s failed=%v", expired, m.FirstNewContentVerdict.Summary(), m.Failed)
		for _, v := range []MetricVerdict{m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
			require.True(t, v.Trusted)
			require.True(t, v.Failed)
		}
		require.True(t, m.Failed)
	}
}

func TestOnlineDecodeFFmpegFailureKinds(t *testing.T) {
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	size, err := decodeKeyframe(data)
	require.NoError(t, err)
	frames := []vp8Frame{{data: data, keyframe: true}, {data: []byte{1}, timestamp: 3000}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	invalid := fullDecode(ctx, frames, size)
	cancel()
	require.Equal(t, DecodeInvalid, invalid.Failure, "%+v", invalid)
	require.NotEmpty(t, invalid.Errors)
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	timedOut := fullDecode(ctx, frames, size)
	cancel()
	require.Equal(t, DecodeUnavailable, timedOut.Failure)
	require.Contains(t, timedOut.Skipped, "deadline exceeded")
	require.Empty(t, timedOut.Errors, "a timeout cannot prove a bitstream error")
	skipped := fullDecode(context.Background(), nil, size)
	require.Equal(t, DecodeUnavailable, skipped.Failure)
	for _, test := range []struct {
		name   string
		result FullDecode
		failed bool
	}{{"invalid", invalid, true}, {"timeout", timedOut, false}, {"skip", skipped, false}} {
		t.Run(test.name, func(t *testing.T) {
			r := testMeasurementRecorder(t)
			r.recording.DecodeEvery = 30
			track := r.tracks[0]
			track.video.sampleResults = []decodeSample{
				{at: 2 * time.Second, decoded: true, sourceAt: 1999 * time.Millisecond},
				{at: 2050 * time.Millisecond, decoded: true, sourceAt: 2049 * time.Millisecond},
				{at: 2100 * time.Millisecond, decoded: true, sourceAt: 2099 * time.Millisecond},
			}
			r.recordOnlineDecode(track, sampleJob{frames: frames, at: 1850 * time.Millisecond, sourceAt: 1848 * time.Millisecond}, test.result)
			m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
			for _, v := range []MetricVerdict{m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
				require.Equal(t, test.failed, v.Trusted, v.Summary())
				require.Equal(t, test.failed, v.Failed, v.Summary())
			}
			require.Equal(t, test.failed, m.Failed)
			if test.failed {
				require.Equal(t, 1, track.video.OnlineDecode.Errors)
				require.Zero(t, track.video.OnlineDecode.Unavailable)
			} else {
				require.Zero(t, track.video.OnlineDecode.Errors)
				require.Equal(t, 1, track.video.OnlineDecode.Unavailable)
			}
		})
	}
}

func TestRecordingSavedCountersAreComputedAtReport(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.start = time.Now().Add(-20 * time.Second)
	r.recording.History = 10 * time.Second
	track := r.tracks[0]
	for i, at := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} {
		r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}}, r.start.Add(at))
	}
	r.move(moveRecord{start: r.start.Add(1500 * time.Millisecond), end: r.start.Add(1800 * time.Millisecond)})
	r.compact(13 * time.Second)
	require.Len(t, r.savedMoves, 1)
	savedPackets := r.savedMoves[0].Tracks[0].PacketsAfter
	savedFailures := r.savedMoves[0].DecryptionFailuresAfterResume
	savedMeasurement := r.savedMoves[0].Measurement
	for i := 0; i < 10; i++ {
		r.packet(track, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i + 3)}}, r.start.Add(14*time.Second+time.Duration(i)*time.Millisecond))
	}
	r.srtpError("failed to verify auth tag")
	r.srtpError("duplicated")
	// Packet ingestion cannot walk or mutate all saved summaries.
	require.Equal(t, savedPackets, r.savedMoves[0].Tracks[0].PacketsAfter)
	require.Equal(t, savedFailures, r.savedMoves[0].DecryptionFailuresAfterResume)
	for i := 0; i < 2; i++ {
		report := r.report().Moves[0]
		require.Equal(t, savedPackets+10, report.Tracks[0].PacketsAfter)
		require.Equal(t, savedFailures+2, report.DecryptionFailuresAfterResume)
		require.Equal(t, savedMeasurement, report.Measurement)
	}
}

// Q3: retained events must add no measurement allocations to a maintenance
// tick. Compare identical histories to avoid a machine-dependent time limit.
func TestRecordingCompactionDoesNotMeasureRetainedEvents(t *testing.T) {
	r := compactionProbeRecorder()
	moves := r.moves
	r.moves = nil
	compact := func() {
		r.lastCompact = 0
		r.compact(130 * time.Second)
	}
	without := testing.AllocsPerRun(2, compact)
	r.moves = moves
	start := time.Now()
	with := testing.AllocsPerRun(2, compact)
	t.Logf("Q3 compact=%s allocations without events=%.0f with events=%.0f", time.Since(start)/3, without, with)
	require.Empty(t, r.savedMoves)
	require.LessOrEqual(t, with, without+32, "retained events must not be remeasured on a compaction tick")
}

func compactionProbeRecorder() *recorder {
	r := newRecorder()
	audio := r.addTrack(kindAudio, 1, 111)
	video := r.addTrack(kindVideo, 2, 96)
	end := 130 * time.Second
	for at := time.Duration(0); at < end; at += 20 * time.Millisecond {
		r.sentAudioUnits[strconv.FormatInt(int64(at), 10)] = contentUnit{at: at, returnedAt: at + time.Millisecond, duration: 20 * time.Millisecond, written: true}
		audio.arrivals = append(audio.arrivals, at+time.Millisecond)
		audio.headers = append(audio.headers, rtpMark{seq: uint16(at / (20 * time.Millisecond))})
	}
	i := 0
	for at := time.Duration(0); at < end; at += time.Second / 30 {
		r.sentVideoFrames[uint16(i&0x7fff)] = sentVideoFrame{identity: uint64(i), at: at, returnedAt: at + time.Millisecond, data: "x", written: true, duration: time.Second / 30}
		for j := 0; j < 4; j++ {
			video.arrivals = append(video.arrivals, at+time.Millisecond)
			video.headers = append(video.headers, rtpMark{seq: uint16(i*4 + j), pictureID: uint16(i & 0x7fff), havePictureID: true})
		}
		video.video.frames = append(video.video.frames, frameMark{at: at + 2*time.Millisecond, receivedAt: at + time.Millisecond, decodable: true, source: sentVideoFrame{identity: uint64(i), at: at}, pictureID: uint16(i & 0x7fff)})
		i++
	}
	r.sentVideo.Frames = i
	for k := 0; k < 4; k++ {
		s := r.start.Add(time.Duration(20+30*k) * time.Second)
		r.moves = append(r.moves, moveRecord{kind: "move", start: s, end: s.Add(50 * time.Millisecond)})
	}
	return r
}

func BenchmarkRecordingCompaction(b *testing.B) {
	r := compactionProbeRecorder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.lastCompact = 0
		r.compact(130 * time.Second)
	}
}
