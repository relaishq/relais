package callharness

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func attackBaseline() []contentUnit {
	var units []contentUnit
	for i := range 40 {
		at := time.Duration(i) * 20 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: fmt.Sprint("b", i)})
	}
	return units
}

// A1: two late units out of three must fail even though fresh arrivals
// repeatedly interrupt the lateness streak.
func TestAttackInterleavedLateLive(t *testing.T) {
	units := attackBaseline()
	start, resumed := time.Second, 1400*time.Millisecond
	for i := range 250 {
		sent := resumed + time.Duration(i)*20*time.Millisecond
		latency := time.Millisecond
		if i%3 != 0 {
			latency = 100 * time.Millisecond
		}
		units = append(units, contentUnit{at: sent, returnedAt: sent + latency, identity: fmt.Sprint("l", i)})
	}
	f := measureFreshness(units, start, resumed, 60*time.Second, 0)
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed, "interleaved lateness cannot become diagnostic-only jitter")
}

// A2: the same stale tail is invisible in a short recording; that recording
// must be unknown, while a complete recording exposes failed recovery.
func TestAttackTruncatedStabilityWindow(t *testing.T) {
	units := attackBaseline()
	start, resumed := time.Second, 1400*time.Millisecond
	for i := range 200 {
		sent := resumed + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: sent, returnedAt: sent + time.Millisecond, identity: fmt.Sprint("l", i)})
	}
	for i := range 15 {
		units = append(units, contentUnit{at: start + time.Duration(i)*20*time.Millisecond, returnedAt: start + 2100*time.Millisecond + time.Duration(i)*20*time.Millisecond, identity: fmt.Sprint("outage", i)})
	}
	short := measureFreshness(units, start, resumed, start+2050*time.Millisecond, 0)
	require.False(t, short.Verdict.Trusted, "hangup/next event cannot conceal an unobserved stale tail")
	require.Contains(t, short.Verdict.Summary(), "stability window truncated")
	complete := measureFreshness(units, start, resumed, 60*time.Second, 0)
	require.True(t, complete.Verdict.Trusted)
	require.True(t, complete.Verdict.Failed)
}

// A3: relative freshness alone cannot accept a permanently buffered path.
func TestAttackConstantDelay(t *testing.T) {
	var units []contentUnit
	start, resumed := time.Second, 1400*time.Millisecond
	for i := range 250 {
		sent := time.Duration(i) * 20 * time.Millisecond
		if sent >= start && sent < resumed-300*time.Millisecond {
			continue
		}
		units = append(units, contentUnit{at: sent, returnedAt: sent + 300*time.Millisecond, identity: fmt.Sprint("constant", i)})
	}
	f := measureFreshness(units, start, resumed, 60*time.Second, 0)
	require.Equal(t, 300*time.Millisecond, f.FirstBaselineMedian)
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed, "a stable 300ms latency must fail the absolute ceiling")
	require.Contains(t, f.Verdict.Summary(), "AbsoluteLag")
}

// A4 uses the production-called decoder record, with real VP8 bytes: no
// outage frame decodes, and a natural keyframe starts decoding at event+950ms.
func TestAttackNaturalKeyframeWait(t *testing.T) {
	r := testMeasurementRecorder(t)
	track := r.tracks[0]
	track.video = &videoRecord{}
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	r.sentFrames[kindVideo][string(data)] = struct{}{}
	r.sentVideoFrames[0] = sentVideoFrame{at: 2448 * time.Millisecond, returnedAt: 2449 * time.Millisecond, data: string(data), written: true}
	size, err := decodeKeyframe(data)
	require.NoError(t, err)
	r.videoFrame(track, &vp8Frame{data: data, keyframe: true, pictureID: 0, firstArrival: r.start.Add(2449 * time.Millisecond), completedAt: r.start.Add(2449 * time.Millisecond)}, size, nil, r.start.Add(2450*time.Millisecond))
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, 950*time.Millisecond, m.FirstNewFrame)
	require.Equal(t, "natural keyframe", m.FirstNewContent)
	require.True(t, m.FirstNewContentVerdict.Trusted)
	require.NotEqual(t, "outage media", m.FirstNewContent, "a natural keyframe wait cannot pass #27's gate")
}

// A5: confirmed wrong bytes are a failure, including when other measurements
// are unknown. They must never fit inside the 20% inconclusive allowance.
func TestAttackIdentityConflict(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint("uncertain=", uncertain), func(t *testing.T) {
			r := testMeasurementRecorder(t)
			v := r.sentVideoFrames[22]
			v.returnedAt = 0
			r.sentVideoFrames[22] = v
			r.contentIdentityErrors = 1
			if uncertain {
				r.hungUpAt = 2 * time.Second
			}
			m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
			for _, verdict := range []MetricVerdict{m.VideoLoss, m.Video.Verdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict} {
				require.True(t, verdict.Trusted)
				require.True(t, verdict.Failed)
				require.Contains(t, verdict.Summary(), "conflicting video content identities")
			}
			require.True(t, m.Failed)
			require.True(t, m.RecoveryFailed())
		})
	}
}

func TestLiveLatenessOverallBudgetAndBoundary(t *testing.T) {
	for _, stride := range []int{9, 10} {
		t.Run(fmt.Sprint("one_late_per_", stride), func(t *testing.T) {
			units := attackBaseline()
			for i := range 180 {
				sent := 1400*time.Millisecond + time.Duration(i)*20*time.Millisecond
				latency := time.Millisecond
				if i%stride == stride-1 {
					latency = 21 * time.Millisecond
				}
				units = append(units, contentUnit{at: sent, returnedAt: sent + latency, identity: fmt.Sprint("budget", i)})
			}
			f := measureFreshness(units, time.Second, 1400*time.Millisecond, 60*time.Second, 0)
			require.True(t, f.Verdict.Trusted)
			require.Equal(t, stride == 9, f.Verdict.Failed, "more than 10%% fails; exactly 10%% passes")
			require.Equal(t, 180, f.LiveUnits)
			require.Equal(t, 180/stride, f.LateLiveUnits)
		})
	}
}

func TestAbsoluteCeilingCanBeConfiguredForNetworkProfile(t *testing.T) {
	units := attackBaseline()
	for i := range units {
		units[i].returnedAt = units[i].at + 300*time.Millisecond
	}
	for i := range 200 {
		at := 1400*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + 300*time.Millisecond, identity: fmt.Sprint("network", i)})
	}
	policy := FreshnessPolicy{AbsoluteLatencyCeiling: 400 * time.Millisecond}
	f := measureFreshness(units, time.Second, 1400*time.Millisecond, 60*time.Second, 0, policy)
	require.True(t, f.Verdict.Trusted)
	require.False(t, f.AbsoluteLag)
	require.False(t, f.Verdict.Failed)
	require.Equal(t, 400*time.Millisecond, f.AbsoluteLatencyCeiling)
	require.Equal(t, 5*time.Second, policy.EvaluationWindow())
}

func TestCorruptedFrameBytesFailThroughCallerRecorder(t *testing.T) {
	r := testMeasurementRecorder(t)
	track := r.tracks[0]
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	data, key, err := src.next()
	require.NoError(t, err)
	require.True(t, key)
	r.sentVideoFrames[0] = sentVideoFrame{at: 1848 * time.Millisecond, data: string(data), written: true}
	wrong := append([]byte(nil), data...)
	wrong[len(wrong)-1] ^= 1
	size, decodeErr := decodeKeyframe(wrong)
	r.videoFrame(track, &vp8Frame{data: wrong, keyframe: true, pictureID: 0}, size, decodeErr, r.start.Add(1850*time.Millisecond))
	require.Positive(t, r.contentIdentityErrors)
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.True(t, m.VideoLoss.Trusted)
	require.True(t, m.VideoLoss.Failed)
	require.True(t, m.RecoveryFailed())
}
