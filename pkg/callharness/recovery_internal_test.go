package callharness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Two inserted replay packets must not disguise a short source-stream margin.
// The first cache packet itself is only 8183 ahead of the last caller packet,
// because the snapshot lagged ten indexes; it still identifies resumed media.
func TestRecoveryExcludesReplayFromSourceMargin(t *testing.T) {
	for _, test := range []struct {
		sequence uint16
		skipped  int
	}{{8305, 8192}, {8303, 8190}} {
		r := &recorder{frameInterval: time.Second / 30, sentVideoFrames: map[uint16]sentVideoFrame{0: {at: time.Millisecond}, 1: {at: 20 * time.Millisecond}}}
		r.tracks = []*trackRecord{{kind: kindVideo,
			arrivals: []time.Duration{2 * time.Millisecond, 21 * time.Millisecond, 22 * time.Millisecond, 23 * time.Millisecond},
			headers:  []rtpMark{{seq: 110, pictureID: 0, havePictureID: true}, {seq: 8293, pictureID: 0, havePictureID: true}, {seq: 8294, pictureID: 0, havePictureID: true}, {seq: test.sequence, pictureID: 1, havePictureID: true}},
			video:    &videoRecord{frames: []frameMark{{at: 22500 * time.Microsecond, firstArrival: 21 * time.Millisecond, source: sentVideoFrame{at: time.Millisecond}, decodable: true, pictureID: 0}}},
		}}
		recovery := r.videoRecovery(10*time.Millisecond, time.Second)
		require.Equal(t, 21*time.Millisecond, recovery.MediaResumedAt)
		require.Equal(t, "Cache", recovery.Path)
		require.Equal(t, 2, recovery.ReplayPackets)
		require.Equal(t, test.skipped, recovery.SourceVideoSkippedSequenceNumbers)
	}
}

func TestRecoveryCommonStartSeparatesLive(t *testing.T) {
	r := &recorder{frameInterval: time.Second / 30, keyframeInterval: time.Second, sentVideoFrames: map[uint16]sentVideoFrame{0: {at: time.Millisecond}, 1: {at: 20 * time.Millisecond, pli: true}}}
	r.tracks = []*trackRecord{{kind: kindVideo, arrivals: []time.Duration{2 * time.Millisecond, 21 * time.Millisecond, 25 * time.Millisecond}, headers: []rtpMark{{seq: 110, pictureID: 0, havePictureID: true}, {seq: 8293, pictureID: 0, havePictureID: true}, {seq: 8400, pictureID: 1, havePictureID: true}}, video: &videoRecord{frames: []frameMark{
		{at: 22 * time.Millisecond, firstArrival: 21 * time.Millisecond, source: sentVideoFrame{at: time.Millisecond}, decodable: true, pictureID: 0},
		{at: 26 * time.Millisecond, firstArrival: 25 * time.Millisecond, source: sentVideoFrame{at: 20 * time.Millisecond, pli: true}, decodable: true, pictureID: 1},
	}}}}
	result := r.videoRecovery(10*time.Millisecond, time.Second)
	require.Equal(t, 12*time.Millisecond, result.FirstDecodedAfterKill)
	require.Equal(t, 16*time.Millisecond, result.FirstDecodedLiveAfterKill)
	require.Equal(t, time.Millisecond, result.FirstDecodedAfterMediaResume)
	require.Equal(t, "Cache", result.Path)
	require.Equal(t, "Keyframe", result.LivePath)
	require.Equal(t, time.Second, result.KeyframeInterval)
}
