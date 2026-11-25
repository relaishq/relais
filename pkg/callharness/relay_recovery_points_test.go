package callharness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These caller observations deliberately include stale and repeated video.
// Relay loss assumes isolation stops old-path media before the restart starts;
// only fresh decoded video and advancing audio after answer application recover.
func recoveryPointCall() *Call {
	r := newRecorder()
	r.tracks = []*trackRecord{
		{kind: kindAudio, arrivals: []time.Duration{time.Second, 2 * time.Second, 6 * time.Second}},
		{kind: kindVideo, arrivals: []time.Duration{1500 * time.Millisecond, 3 * time.Second, 5 * time.Second}, video: &videoRecord{frames: []frameMark{
			{at: 3 * time.Second, decodable: true},
			{at: 4 * time.Second, decodable: true, repeated: true},
			{at: 4500 * time.Millisecond},
			{at: 5 * time.Second, decodable: true},
		}}},
	}
	return &Call{rec: r}
}

func TestRelayRecoveryGapCharacterization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Call)
		want   time.Duration
	}{
		{"fresh audio is later than decoded video", func(*Call) {}, 4 * time.Second},
		{"video is later than audio", func(c *Call) { c.rec.tracks[0].arrivals[2] = 4 * time.Second }, 3 * time.Second},
		{"no post-commit audio", func(c *Call) { c.rec.tracks[0].arrivals = c.rec.tracks[0].arrivals[:2] }, 0},
		{"no fresh decoded video", func(c *Call) { c.rec.tracks[1].video.frames = c.rec.tracks[1].video.frames[:3] }, 0},
		{"no media before loss", func(c *Call) {
			c.rec.tracks[0].arrivals = c.rec.tracks[0].arrivals[2:]
			c.rec.tracks[1].arrivals = c.rec.tracks[1].arrivals[1:]
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := recoveryPointCall()
			tc.mutate(c)
			// Arrival exactly at loss is excluded; arrivals exactly at commit count.
			require.Equal(t, tc.want, c.RelayRecoveryGap(c.rec.start.Add(3*time.Second), c.rec.start.Add(4*time.Second)))
		})
	}
}

func TestRelayRecoveryPointsExposePartialEvidence(t *testing.T) {
	c := recoveryPointCall()
	last, video, audio := c.RelayRecoveryPoints(c.rec.start.Add(3*time.Second), c.rec.start.Add(4*time.Second))
	require.Equal(t, 2*time.Second, last)
	require.Equal(t, 5*time.Second, video)
	require.Equal(t, 6*time.Second, audio)
	c.rec.tracks[0].arrivals = c.rec.tracks[0].arrivals[:2]
	last, video, audio = c.RelayRecoveryPoints(c.rec.start.Add(3*time.Second), c.rec.start.Add(4*time.Second))
	require.Equal(t, 2*time.Second, last)
	require.Equal(t, 5*time.Second, video)
	require.Zero(t, audio)
	require.Zero(t, c.RelayRecoveryGap(c.rec.start.Add(3*time.Second), c.rec.start.Add(4*time.Second)))
}
