package callharness

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

// Model #27's at-least-once input replay: already echoed pictures precede
// unseen outage deltas. A complete repeat must not reset the receiver chain
// or enter ffmpeg again. Also interrupt the first unseen picture as in Q1.
func TestRepeatedPicturesPreserveOutageDecode(t *testing.T) {
	for _, replayFrom := range []int{20, 0} {
		for _, online := range []bool{false, true} {
			for _, partial := range []bool{false, true} {
				t.Run(fmt.Sprintf("from=%d/online=%t/partial=%t", replayFrom, online, partial), func(t *testing.T) {
					r := newRecorder()
					r.frameInterval = time.Second / 30
					track := r.addTrack(kindVideo, 1, 96)
					var sampler *onlineSampler
					if online {
						r.recording.DecodeEvery = 30
						sampler = &onlineSampler{call: &Call{rec: r}, track: track, queue: make(chan sampleJob, onlineDecodeQueueCapacity)}
					}
					src, err := newVP8Source(callerVideo)
					require.NoError(t, err)
					payloader := &codecs.VP8Payloader{EnablePictureID: true}
					payloads := make([][][]byte, 60)
					for i := range payloads {
						data, key, err := src.next()
						require.NoError(t, err)
						data = r.sendingVideo(data, nil, time.Second/30)
						id := uint16(i)
						source := r.sentVideoFrames[id]
						source.at = 500*time.Millisecond + time.Duration(i)*(time.Second/30)
						if i >= 30 {
							source.at = 1910*time.Millisecond + time.Duration(i-30)*(time.Second/30)
						}
						r.sentVideoFrames[id] = source
						r.sent(kindVideo, key)
						payloads[i] = payloader.Payload(100, data)
					}
					assembler := &vp8Assembler{}
					seq := uint16(100)
					incomplete := 0
					deliver := func(i, first, count int, at time.Duration) {
						for j := first; j < first+count; j++ {
							packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: j == len(payloads[i])-1}, Payload: payloads[i][j]}
							seq++
							arrived := r.start.Add(at)
							r.packet(track, packet, arrived)
							frames, n := assembler.pushAll(packet, arrived)
							incomplete += n
							for _, frame := range frames {
								size, decodeErr, repeated := r.decodeVideoFrame(track, frame, arrived.Add(time.Microsecond))
								if repeated {
									continue
								}
								require.NoError(t, decodeErr)
								accepted := r.videoFrame(track, frame, size, decodeErr, arrived.Add(time.Microsecond))
								if accepted && sampler != nil {
									sampler.frame(frame, size, decodeErr)
									// Drain synchronously to exercise real ffmpeg, not queue scheduling.
									for len(sampler.queue) > 0 {
										job := <-sampler.queue
										ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
										result := fullDecode(ctx, job.frames, job.size)
										cancel()
										r.recordOnlineDecode(track, job, result)
									}
								}
							}
						}
					}
					for i := 0; i <= 26; i++ {
						deliver(i, 0, len(payloads[i]), r.sentVideoFrames[uint16(i)].at+time.Millisecond)
					}
					if partial {
						require.Greater(t, len(payloads[27]), 2)
						deliver(27, 0, 2, 1401*time.Millisecond)
					}
					seq += 8192
					for i := replayFrom; i < 30; i++ {
						deliver(i, 0, len(payloads[i]), 1800*time.Millisecond+time.Duration(i-replayFrom)*time.Millisecond)
					}
					for i := 30; i < len(payloads); i++ {
						deliver(i, 0, len(payloads[i]), r.sentVideoFrames[uint16(i)].at+time.Millisecond)
					}
					r.hungUpAt = 7 * time.Second
					move := MoveReport{Kind: "takeover", Start: 1450 * time.Millisecond, End: 1750 * time.Millisecond}
					move.Recovery = r.videoRecovery(move.Start, r.hungUpAt)
					m := r.eventMeasurement(move, r.hungUpAt, 0)
					frames, size := r.decodeInput(0)
					var full FullDecode
					if !online {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						full = fullDecode(ctx, frames, size)
						cancel()
					}
					t.Logf("REPEATED_PICTURES partial=%t first_new=%q undecodable=%d gaps=%d nonmonotonic=%d full_decode=%d/%d errors=%q", partial, m.FirstNewContent, track.video.UndecodableFrames, track.video.FrameGaps, track.video.NonMonotonicTimestamps, full.FramesDecoded, full.FramesIn, full.Errors)
					require.Equal(t, "outage media", m.FirstNewContent, m.Summary())
					require.True(t, m.FirstNewContentVerdict.Trusted, m.Summary())
					require.Equal(t, uint64(27), m.FirstNewContentIdentity)
					require.Zero(t, m.LostVideoFrames)
					require.Zero(t, track.video.UndecodableFrames)
					require.Zero(t, track.video.NonMonotonicTimestamps)
					require.Zero(t, track.video.FrameGaps)
					require.Equal(t, 27-replayFrom, track.video.RepeatedFrames)
					require.True(t, move.Recovery.FirstDecodedRepeated)
					require.Len(t, r.repeatedVideoReturns, 27-replayFrom, "content accounting still observes complete repeats")
					require.Equal(t, 60, track.video.DecodableFrames)
					require.Equal(t, 2, track.video.KeyframesDecoded, "a repeated keyframe never re-enters the decoder")
					if online {
						require.Empty(t, frames)
						d := track.video.OnlineDecode
						require.Zero(t, d.Errors)
						require.Zero(t, d.Dropped)
						require.Zero(t, d.Unavailable)
						require.True(t, d.Conclusive(), "%+v", d)
						require.Equal(t, d.Sampled, d.UniqueDecoded)
						require.Equal(t, minimumDecodedSamples, r.eventDecodedSamples(move.Start, 1850*time.Millisecond), "unseen outage deltas are all checked; repeats do not count as samples")
					} else {
						require.Len(t, frames, 60, "decoder input excludes already decoded pictures")
						require.Empty(t, full.Skipped)
						require.Empty(t, full.Errors)
						require.Equal(t, 60, full.FramesDecoded)
					}
					if partial {
						require.Equal(t, 1, incomplete)
					} else {
						require.Zero(t, incomplete)
					}
				})
			}
		}
	}
}

// Repeat filtering requires the full known identity and a successful earlier
// decode. A failed keyframe, old epoch or changed payload must not disappear.
func TestRepeatedPicturesRequireDecodedExactIdentity(t *testing.T) {
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	for _, test := range []string{"failed decode", "different epoch", "different payload", "exact repeat"} {
		t.Run(test, func(t *testing.T) {
			r := newRecorder()
			track := r.addTrack(kindVideo, 1, 96)
			original := identifyVP8(data, 0)
			r.sentVideoFrames[0] = sentVideoFrame{identity: 0, at: time.Second, data: string(original), written: true}
			f := &vp8Frame{data: original, keyframe: true, pictureID: 0, completedAt: r.start.Add(time.Second + time.Millisecond)}
			size, decodeErr, repeated := r.decodeVideoFrame(track, f, f.completedAt)
			require.False(t, repeated)
			require.NoError(t, decodeErr)
			if test == "failed decode" {
				decodeErr = errors.New("invalid VP8 bitstream")
			}
			require.True(t, r.videoFrame(track, f, size, decodeErr, f.completedAt))
			replay := *f
			replay.firstSeq = 8192
			replay.lastSeq = 8192
			replay.completedAt = r.start.Add(2 * time.Second)
			switch test {
			case "different epoch":
				replay.data = identifyVP8(data, 32768)
				r.sentVideoFrames[0] = sentVideoFrame{identity: 32768, at: 1500 * time.Millisecond, data: string(replay.data), written: true}
			case "different payload":
				replay.data = append(bytes.Clone(original), 1)
			}
			size, decodeErr, repeated = r.decodeVideoFrame(track, &replay, replay.completedAt)
			require.Equal(t, test == "exact repeat", repeated)
			if repeated {
				require.Equal(t, 1, track.video.RepeatedFrames)
				require.Zero(t, size)
			} else {
				require.Zero(t, track.video.RepeatedFrames)
				require.True(t, r.videoFrame(track, &replay, size, decodeErr, replay.completedAt))
				if test == "different payload" {
					require.Equal(t, 1, r.contentIdentityErrors)
				}
				if test == "different epoch" {
					require.Equal(t, uint64(32768), track.video.decodedPictures[0].identity)
				}
				if test == "failed decode" {
					require.Equal(t, 1, track.video.DecodableFrames, "a successfully retried picture is accepted")
				}
			}
		})
	}
}

func TestRecordingDecodedPicturesExpire(t *testing.T) {
	r := newRecorder()
	r.recording.History = 15 * time.Second
	track := r.addTrack(kindVideo, 1, 96)
	track.video.decodedPictures = map[uint16]sentVideoFrame{0: {at: time.Second, data: "old"}, 1: {at: 10 * time.Second, data: "recent"}}
	require.Equal(t, 2, r.memoryGauge().DecodedPictures)
	r.compact(20 * time.Second)
	require.Equal(t, 1, r.memoryGauge().DecodedPictures)
	require.NotContains(t, track.video.decodedPictures, uint16(0))
	require.Contains(t, track.video.decodedPictures, uint16(1))
}

func TestVP8IVFUsesSourceClock(t *testing.T) {
	// Include relay timestamps that move backward, then cross their 32-bit wrap.
	frames := []vp8Frame{
		{data: []byte{1}, timestamp: 9000, sourceAt: 20 * time.Hour},
		{data: []byte{2}, timestamp: 6000, sourceAt: 20*time.Hour + time.Second},
		{data: []byte{3}, timestamp: 0, sourceAt: 20*time.Hour + 2*time.Second},
	}
	var ivf bytes.Buffer
	writeIVF(&ivf, frames, image.Point{X: 1, Y: 1})
	for i := range frames {
		offset := 32 + i*13
		require.Equal(t, uint64(i*vp8ClockRate), binary.LittleEndian.Uint64(ivf.Bytes()[offset+4:]))
	}
}

// Discarding an old decoded picture cannot repair a genuinely missing source
// reference. Keep the loss and undecodable checks strict across such a replay.
func TestRepeatedPicturesDoNotRepairMissingReference(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	frames := make([]vp8Frame, 5)
	for i := range frames {
		data, key, err := src.next()
		require.NoError(t, err)
		data = r.sendingVideo(data, nil, time.Second/30)
		source := r.sentVideoFrames[uint16(i)]
		source.at = time.Second + time.Duration(i)*time.Second/30
		r.sentVideoFrames[uint16(i)] = source
		r.sent(kindVideo, key)
		frames[i] = vp8Frame{data: data, keyframe: key, pictureID: uint16(i), timestamp: uint32(i * 3000), firstSeq: uint16(i), lastSeq: uint16(i)}
	}
	deliver := func(i int, at time.Duration) bool {
		f := frames[i]
		f.completedAt = r.start.Add(at)
		size, err, repeated := r.decodeVideoFrame(track, &f, f.completedAt)
		require.NoError(t, err)
		if repeated {
			return false
		}
		return r.videoFrame(track, &f, size, err, f.completedAt)
	}
	require.True(t, deliver(0, 1100*time.Millisecond))
	require.True(t, deliver(1, 1150*time.Millisecond))
	require.True(t, deliver(3, 1250*time.Millisecond)) // frame 2 never arrived
	require.False(t, track.video.chain)
	require.False(t, deliver(1, 1300*time.Millisecond))
	require.False(t, track.video.chain)
	require.True(t, deliver(4, 1350*time.Millisecond))
	require.Equal(t, 1, track.video.RepeatedFrames)
	require.Equal(t, 1, track.video.FrameGaps)
	require.Equal(t, 2, track.video.UndecodableFrames)
	require.Equal(t, 2, track.video.DecodableFrames)
	require.False(t, track.video.chain)
}
