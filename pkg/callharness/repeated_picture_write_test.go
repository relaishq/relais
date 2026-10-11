package callharness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A complete echo can reach the reader before the source write returns.
// Replay it after frame 16: neither a keyframe nor a delta may reenter the
// decoder once that write has completed and the earlier decode is known.
func TestRepeatedPictureAfterWriteCompletion(t *testing.T) {
	for _, delayedWrite := range []int{0, 7} {
		t.Run(fmt.Sprint(delayedWrite), func(t *testing.T) {
			r := newRecorder()
			track := r.addTrack(kindVideo, 1, 96)
			src, err := newVP8Source(callerVideo)
			require.NoError(t, err)
			frames := make([]vp8Frame, 21)
			deliver := func(f vp8Frame, at time.Duration) {
				f.completedAt = r.start.Add(at)
				size, decodeErr, repeated := r.decodeVideoFrame(track, &f, f.completedAt)
				require.NoError(t, decodeErr)
				if !repeated {
					r.videoFrame(track, &f, size, decodeErr, f.completedAt)
				}
			}
			for i := range frames {
				data, key, err := src.next()
				require.NoError(t, err)
				data = r.sendingVideo(data, nil, time.Second/30)
				source := r.sentVideoFrames[uint16(i)]
				source.at = time.Second + time.Duration(i)*time.Second/30
				r.sentVideoFrames[uint16(i)] = source
				frames[i] = vp8Frame{data: data, keyframe: key, pictureID: uint16(i), timestamp: uint32(i * 3000), firstSeq: uint16(i), lastSeq: uint16(i)}
				if i != delayedWrite {
					r.sent(kindVideo, key)
				}
				if i <= 16 {
					deliver(frames[i], time.Second+time.Duration(i)*time.Second/30)
				}
				if i == delayedWrite {
					r.sent(kindVideo, key)
				}
			}
			replay := frames[delayedWrite]
			replay.firstSeq, replay.lastSeq = 8192, 8192
			deliver(replay, 2*time.Second)
			for i := 17; i < len(frames); i++ {
				deliver(frames[i], 2100*time.Millisecond+time.Duration(i-17)*time.Second/30)
			}
			inputs, size := r.decodeInput(0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			full := fullDecode(ctx, inputs, size)
			t.Logf("WRITE_COMPLETION_REPEAT repeats=%d full_decode=%d/%d errors=%q", track.video.RepeatedFrames, full.FramesDecoded, full.FramesIn, full.Errors)
			require.Empty(t, full.Skipped)
			require.Empty(t, full.Errors)
			require.Equal(t, len(frames), full.FramesDecoded)
			require.Equal(t, 1, track.video.RepeatedFrames)
			require.Zero(t, track.video.UndecodableFrames)
			require.Zero(t, track.video.FrameGaps)
		})
	}
}
