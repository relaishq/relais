package callharness

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func freshnessPolicyUnits() []contentUnit {
	var units []contentUnit
	for i := range 40 {
		at := time.Duration(i) * 20 * time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: fmt.Sprint("baseline", i)})
	}
	for i := range 3 {
		at := 1400*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: at, returnedAt: at + time.Millisecond, identity: fmt.Sprint("fresh", i)})
	}
	return units
}

func TestFreshnessIgnoresIsolatedLiveJitterAndLaterSpikes(t *testing.T) {
	units := freshnessPolicyUnits()
	for i, relative := range []time.Duration{1500 * time.Millisecond, 10800 * time.Millisecond, 44700 * time.Millisecond} {
		arrival := time.Second + relative
		units = append(units, contentUnit{at: arrival - []time.Duration{21 * time.Millisecond, 12700 * time.Microsecond, 21700 * time.Microsecond}[i], returnedAt: arrival, identity: fmt.Sprint("spike", i)})
		units = append(units, contentUnit{at: arrival + 20*time.Millisecond, returnedAt: arrival + 21*time.Millisecond, identity: fmt.Sprint("following", i)})
	}
	f := measureFreshness(units, time.Second, 1400*time.Millisecond, 60*time.Second, 0)
	require.True(t, f.Verdict.Trusted)
	require.False(t, f.Verdict.Failed)
	require.Equal(t, 401*time.Millisecond, f.BackToBaseline)
	require.Equal(t, 5, f.PostEventSamples, "later units are not attributed to the event")
	require.Equal(t, 1, f.JitterUnits)
	require.Equal(t, 10*time.Millisecond, f.JitterMaxExcess)
	require.Equal(t, 6*time.Second, f.EvaluationEnd)
	require.Equal(t, 21*time.Millisecond, f.PeakLatency, "later spikes must not change event peak")
}

func TestFreshnessSustainedLiveLatenessBreaksRecovery(t *testing.T) {
	units := freshnessPolicyUnits()
	for i := range 3 {
		arrival := 2500*time.Millisecond + time.Duration(i)*20*time.Millisecond
		units = append(units, contentUnit{at: arrival - 21*time.Millisecond, returnedAt: arrival, identity: fmt.Sprint("late", i)})
	}
	f := measureFreshness(units, time.Second, 1400*time.Millisecond, 60*time.Second, 0)
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed)
	require.False(t, f.Recovered)
	require.Zero(t, f.JitterUnits, "sustained lateness is not isolated jitter")
}

func TestFreshnessPreResumeContentBreaksEvenBelowTolerance(t *testing.T) {
	units := freshnessPolicyUnits()
	units = append(units, contentUnit{at: 1399 * time.Millisecond, returnedAt: 1405 * time.Millisecond, identity: "stale"})
	f := measureFreshness(units, time.Second, 1400*time.Millisecond, 60*time.Second, 0)
	require.True(t, f.Verdict.Trusted)
	require.True(t, f.Verdict.Failed, "only two fresh units follow the pre-resume return")
}

func TestFreshnessPolicyConfiguresProductionMeasurement(t *testing.T) {
	r := testMeasurementRecorder(t)
	r.freshnessPolicy = FreshnessPolicy{RecoveryLimit: 200 * time.Millisecond, StabilityWindow: 700 * time.Millisecond}
	m := r.eventMeasurement(testMeasurementMove(), r.hungUpAt, 0)
	require.Equal(t, 200*time.Millisecond, m.Audio.RecoveryLimit)
	require.Equal(t, 700*time.Millisecond, m.Audio.StabilityWindow)
	require.Equal(t, testMeasurementMove().Start+900*time.Millisecond, m.Audio.EvaluationEnd)
	require.True(t, m.Audio.Verdict.Failed, "recovery at 301ms passes default 2s but exceeds this 200ms limit")
}
