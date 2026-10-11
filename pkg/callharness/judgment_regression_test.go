package callharness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// P1: freshness recovery latches on the first 3 fresh units; stale media
// arriving later (up to 2.5 s after the event) does not move BackToBaseline.
func TestProbeFreshnessLatchesEarly(t *testing.T) {
	var units []contentUnit
	for i := 0; i < 40; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: "b" + string(rune(i))})
	}
	start, resumed := time.Second, 1400*time.Millisecond
	// three fresh live units right at resume
	for i := 0; i < 3; i++ {
		at := resumed + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: "live" + string(rune(i))})
	}
	// then paced outage replay: 20 stale units arriving until start+2.4s
	for i := 0; i < 20; i++ {
		sent := start + time.Duration(i)*20*time.Millisecond
		arr := resumed + 100*time.Millisecond + time.Duration(i)*100*time.Millisecond
		units = append(units, contentUnit{at: sent, returnedAt: arr, identity: "out" + string(rune(i))})
	}
	f := measureFreshness(units, start, resumed, 5*time.Second, 0)
	require.False(t, f.Recovered, "stale replay after an early fresh burst breaks recovery")
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed)
}

// P2: permanent lag after the event -> reported inconclusive, not failed.
func TestProbePermanentLagIsInconclusive(t *testing.T) {
	r := testMeasurementRecorder(t)
	for k, u := range r.sentAudioUnits {
		if u.at >= 1500*time.Millisecond {
			u.returnedAt = u.at + 400*time.Millisecond
			r.sentAudioUnits[k] = u
		}
	}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.True(t, m.Audio.Verdict.Trusted)
	require.True(t, m.Audio.Verdict.Failed)
	require.False(t, m.Inconclusive)
}

// P2b: real lost audio, but a noisy video baseline makes the whole event
// inconclusive, so assertNoLostContent would skip.
func TestProbeLossMaskedByVideoNoise(t *testing.T) {
	r := testMeasurementRecorder(t)
	for k, u := range r.sentAudioUnits {
		if u.at >= 1500*time.Millisecond && u.at < 1800*time.Millisecond {
			u.returnedAt = 0
			r.sentAudioUnits[k] = u
		}
	}
	for id, f := range r.sentVideoFrames {
		if f.at == time.Second || f.at == 1050*time.Millisecond {
			f.returnedAt = f.at + 100*time.Millisecond
			r.sentVideoFrames[id] = f
		}
	}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, 120*time.Millisecond, m.LostAudio)
	require.True(t, m.AudioLoss.Trusted, "video baseline noise cannot hide audio loss")
	require.True(t, m.AudioLoss.Failed)
}

// P2c: video never decodes after resume -> inconclusive.
func TestProbeNoVideoIsInconclusive(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.tracks[0].video.frames = nil
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.True(t, m.FirstNewContentVerdict.Trusted)
	require.True(t, m.FirstNewContentVerdict.Failed)
	require.False(t, m.Inconclusive)
}

// P3: keyframe requested by the OLD worker before the event, response sent
// during the outage and replayed by a relay buffer.
func TestProbePreEventRequestResponse(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.tracks[0].video.frames[0].source = sentVideoFrame{at: 1510 * time.Millisecond, pli: true, requestedAt: 1490 * time.Millisecond}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, "outage media", m.FirstContent)
	require.Equal(t, "outage media", m.FirstNewContent)
}

// P4: frame sent just before the kill, never delivered by the old worker,
// replayed by a relay buffer.
func TestProbeInFlightPreEventIsCache(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.tracks[0].video.frames[0].source = sentVideoFrame{at: 1490 * time.Millisecond, returnedAt: 1850 * time.Millisecond}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, "outage media", m.FirstContent)
	require.Equal(t, "outage media", m.FirstNewContent)
}

// P5: live frame sent after the route switched but ~RTT before the caller
// observed resume is "outage media".
func TestProbeLiveBeforeObservedResume(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.tracks[0].video.frames[0].source = sentVideoFrame{at: 1795 * time.Millisecond, returnedAt: 1800 * time.Millisecond}
	// Keep the original 1850ms decode time; actual packet completion was
	// 1800ms. Decoder work must not inflate a normal 5ms round trip.
	r.tracks[0].video.frames[0].receivedAt = 1800 * time.Millisecond
	r.tracks[0].video.frames[0].firstArrival = 1800 * time.Millisecond
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, "live", m.FirstContent)
	require.Equal(t, "live", m.FirstNewContent)
}
