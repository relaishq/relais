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
	r := &recorder{frameInterval: time.Second / 30, keyframeInterval: time.Second, sentVideoFrames: map[uint16]sentVideoFrame{0: {at: time.Millisecond}, 1: {at: 24 * time.Millisecond, pli: true}}}
	r.tracks = []*trackRecord{{kind: kindVideo, arrivals: []time.Duration{2 * time.Millisecond, 21 * time.Millisecond, 25 * time.Millisecond}, headers: []rtpMark{{seq: 110, pictureID: 0, havePictureID: true}, {seq: 8293, pictureID: 0, havePictureID: true}, {seq: 8400, pictureID: 1, havePictureID: true}}, video: &videoRecord{frames: []frameMark{
		{at: 22 * time.Millisecond, firstArrival: 21 * time.Millisecond, source: sentVideoFrame{at: time.Millisecond}, decodable: true, pictureID: 0},
		{at: 26 * time.Millisecond, firstArrival: 25 * time.Millisecond, source: sentVideoFrame{at: 24 * time.Millisecond, pli: true}, decodable: true, pictureID: 1},
	}}}}
	result := r.videoRecovery(10*time.Millisecond, time.Second)
	require.Equal(t, 12*time.Millisecond, result.FirstDecodedAfterKill)
	require.Equal(t, 16*time.Millisecond, result.FirstDecodedLiveAfterKill)
	require.Equal(t, time.Millisecond, result.FirstDecodedAfterMediaResume)
	require.Equal(t, "Cache", result.Path)
	require.Equal(t, "Keyframe", result.LivePath)
	require.Equal(t, time.Second, result.KeyframeInterval)
}

func TestRecoveryOutageFrameIsNotFirstLive(t *testing.T) {
	r := &recorder{sentVideoFrames: map[uint16]sentVideoFrame{0: {at: time.Millisecond}, 1: {at: 20 * time.Millisecond, pli: true}}, frameInterval: time.Second / 30}
	r.tracks = []*trackRecord{{kind: kindVideo, arrivals: []time.Duration{2 * time.Millisecond, 21 * time.Millisecond}, headers: []rtpMark{{seq: 110, pictureID: 0}, {seq: 8303, pictureID: 1, havePictureID: true}}, video: &videoRecord{frames: []frameMark{{at: 22 * time.Millisecond, firstArrival: 21 * time.Millisecond, source: sentVideoFrame{at: 20 * time.Millisecond, pli: true}, pictureID: 1, decodable: true}}}}}
	result := r.videoRecovery(10*time.Millisecond, time.Second)
	require.Equal(t, 21*time.Millisecond, result.MediaResumedAt)
	require.Equal(t, 12*time.Millisecond, result.FirstDecodedAfterKill)
	require.Zero(t, result.FirstDecodedLiveAfterKill, "sent after kill but before resume is outage content")
	require.Empty(t, result.LivePath)
}

func TestRecoveryTracksDecodedPLIIndependently(t *testing.T) {
	for _, test := range []struct {
		name      string
		sent      time.Duration
		requested time.Duration
		pli       bool
		decodable bool
		repeated  bool
		want      time.Duration
	}{
		{"request after resume", 24 * time.Millisecond, 23 * time.Millisecond, true, true, false, 16 * time.Millisecond},
		{"request during outage", 20 * time.Millisecond, 19 * time.Millisecond, true, true, false, 16 * time.Millisecond},
		{"natural keyframe alone", 24 * time.Millisecond, 0, false, true, false, 0},
		{"request without decode", 24 * time.Millisecond, 23 * time.Millisecond, true, false, false, 0},
		{"request before kill", 24 * time.Millisecond, 9 * time.Millisecond, true, true, false, 0},
		{"request after send", 24 * time.Millisecond, 25 * time.Millisecond, true, true, false, 0},
		{"repeated picture", 24 * time.Millisecond, 23 * time.Millisecond, true, true, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := sentVideoFrame{at: test.sent, requestedAt: test.requested, pli: test.pli}
			r := &recorder{sentVideoFrames: map[uint16]sentVideoFrame{0: {at: time.Millisecond}, 1: {at: 20 * time.Millisecond}, 2: response}}
			r.tracks = []*trackRecord{{kind: kindVideo,
				arrivals: []time.Duration{2 * time.Millisecond, 21 * time.Millisecond, 25 * time.Millisecond},
				headers:  []rtpMark{{seq: 110, pictureID: 0, havePictureID: true}, {seq: 8303, pictureID: 1, havePictureID: true}, {seq: 8304, pictureID: 2, havePictureID: true}},
				video: &videoRecord{frames: []frameMark{
					{at: 22 * time.Millisecond, source: sentVideoFrame{at: 20 * time.Millisecond}, decodable: true, pictureID: 1},
					{at: 26 * time.Millisecond, source: response, decodable: test.decodable, repeated: test.repeated, pictureID: 2},
				}},
			}}
			recovery := r.videoRecovery(10*time.Millisecond, time.Second)
			require.Equal(t, "Live", recovery.Path)
			require.Equal(t, 12*time.Millisecond, recovery.FirstDecodedAfterKill)
			require.Zero(t, recovery.ReplayPackets)
			require.Equal(t, test.want, recovery.FirstDecodedPLIAfterKill)
		})
	}
}
