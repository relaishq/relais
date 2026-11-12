package callharness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The caller received one frame 0.128 ms after the kill estimate, inside the
// kill-time interval. No clock bound decides whether that receipt came first.
func frameInFlightAtKill(t *testing.T) *recorder {
	t.Helper()
	r, _ := classificationFixture(t, 0)
	kill := 1500 * time.Millisecond
	tracker, err := NewClockTracker(0)
	require.NoError(t, err)
	at := r.start.Add(kill)
	require.NoError(t, tracker.Add(ClockProbe{CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at}))
	r.distributed.Clock = tracker
	r.distributed.ControlOffset = OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond}
	r.distributed.WorkerOffset = OffsetInterval{Lower: -3 * time.Millisecond, Upper: 3 * time.Millisecond}
	r.tracks[0].video.frames[0].source = sentVideoFrame{at: 1490 * time.Millisecond, returnedAt: kill + 128*time.Microsecond, written: true, identity: 100}
	return r
}

// The operator media judge (internal/operatorevidence/judge-src) reads these
// serialized reasons. They characterize the classification it relies on: when
// every evaluated kill time passes and only the label differs, the undecided
// verdict carries the interval reason and its duration bounds, nothing else.
func TestFrameInFlightAtKillLeavesOnlyIntervalReasons(t *testing.T) {
	r := frameInFlightAtKill(t)
	move := r.measureMoves(r.hungUpAt, 1)[0]
	m := move.Measurement
	for name, v := range map[string]MetricVerdict{"first_content": m.FirstContentVerdict, "first_new_content": m.FirstNewContentVerdict} {
		require.False(t, v.Trusted, name)
		require.False(t, v.Failed, name)
		require.Len(t, v.Reasons, 2, name)
		require.Contains(t, v.Reasons[0], name+" classification changes within event-time interval; width=6ms")
		require.Contains(t, v.Reasons[1], "duration=[")
	}
	require.True(t, m.Inconclusive)
	require.Equal(t, "inconclusive", move.Distributed.WindowOutcome)
	labels := map[string]bool{}
	for _, c := range move.Distributed.Timing.Candidates {
		require.True(t, c.Trusted && !c.Failed, c.Metric+" "+c.Label)
		if c.Metric == "first_content" {
			labels[c.Label] = true
		}
	}
	require.Equal(t, map[string]bool{"cache": true, "outage media": true}, labels)
	require.Zero(t, move.Distributed.Timing.OmittedCandidates)
}

// With no later decodable frame, a receipt before the kill leaves no new
// content. That failing kill time adds its own reason before the interval one.
func TestFrameInFlightAtKillKeepsAFailingKillTimeReason(t *testing.T) {
	r := frameInFlightAtKill(t)
	r.tracks[0].video.frames = r.tracks[0].video.frames[:1]
	m := r.measureMoves(r.hungUpAt, 1)[0].Measurement
	v := m.FirstNewContentVerdict
	require.False(t, v.Trusted)
	require.False(t, v.Failed)
	require.Equal(t, "no decoded new content after resume before limit", v.Reasons[0])
	require.Contains(t, v.Reasons[1], "first_new_content classification changes within event-time interval;")
	// The first decoded frame itself still passes for every kill time.
	require.Len(t, m.FirstContentVerdict.Reasons, 2)
	require.Contains(t, m.FirstContentVerdict.Reasons[0], "first_content classification changes within event-time interval;")
}
