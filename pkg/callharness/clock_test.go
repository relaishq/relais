package callharness

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func probeAt(t time.Time, skew, rtt time.Duration) ClockProbe {
	return ClockProbe{CallerSend: t, CallerReceive: t.Add(rtt), ServerReceive: t.Add(skew + rtt/3), ServerSend: t.Add(skew + 2*rtt/3)}
}
func TestClockInterval(t *testing.T) {
	at := time.Now()
	p := probeAt(at, 3*time.Second, 3*time.Millisecond)
	i, err := p.Offset()
	require.NoError(t, err)
	require.Equal(t, OffsetInterval{Lower: 3*time.Second - time.Millisecond, Upper: 3*time.Second + time.Millisecond}, i)
	i, err = i.Compose(OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond})
	require.NoError(t, err)
	wide, err := i.Widen(10*time.Second, 100)
	require.NoError(t, err)
	require.Equal(t, OffsetInterval{Lower: 3*time.Second - 4*time.Millisecond, Upper: 3*time.Second + 4*time.Millisecond}, wide)
	mapped := wide.Map(at.Add(3 * time.Second))
	require.Equal(t, at.Add(-4*time.Millisecond), mapped.Earliest)
	require.Equal(t, at.Add(4*time.Millisecond), mapped.Latest)
	for _, ppm := range []float64{-1, 1e6} {
		_, err := i.Widen(time.Second, ppm)
		require.Error(t, err)
	}
}
func TestClockRejectsJumps(t *testing.T) {
	at := time.Now()
	tracker, err := NewClockTracker(100)
	require.NoError(t, err)
	require.Equal(t, float64(100), tracker.DriftPPM())
	require.NoError(t, tracker.Add(probeAt(at, 0, time.Millisecond)))
	require.ErrorContains(t, tracker.Add(probeAt(at.Add(time.Second), time.Second, time.Millisecond)), "jump")
	_, err = tracker.Map(at, OffsetInterval{})
	require.ErrorContains(t, err, "jump")
	// A later good probe cannot repair an invalidated run.
	require.Error(t, tracker.Add(probeAt(at.Add(2*time.Second), 0, time.Millisecond)))
	p := probeAt(at, 0, time.Millisecond)
	p.ServerSend = p.ServerReceive.Add(time.Second)
	_, err = p.Offset()
	require.Error(t, err)
	// Carry a monotonic timestamp across a real caller wall-clock jump.
	p = probeAt(at, 0, time.Millisecond)
	p.CallerReceive = p.CallerReceive.Round(0).Add(-time.Second)
	_, err = p.Offset()
	require.Error(t, err)
}
func TestClockDriftAndComposition(t *testing.T) {
	at := time.Now()
	tracker, err := NewClockTracker(100)
	require.NoError(t, err)
	require.Equal(t, float64(100), tracker.DriftPPM())
	require.NoError(t, tracker.Add(probeAt(at, 2*time.Second, 3*time.Millisecond)))
	require.NoError(t, tracker.Add(probeAt(at.Add(10*time.Second), 2*time.Second+500*time.Microsecond, 3*time.Millisecond)))
	mapped, err := tracker.Map(at.Add(22*time.Second), OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond})
	require.NoError(t, err)
	require.True(t, mapped.Earliest.Before(at.Add(20*time.Second)))
	require.True(t, mapped.Latest.After(at.Add(20*time.Second)))
	require.Greater(t, mapped.Latest.Sub(mapped.Earliest), 4*time.Millisecond)
}

func TestClockOverflowIsUnknown(t *testing.T) {
	max := time.Duration(1<<63 - 1)
	_, err := (OffsetInterval{Lower: max, Upper: max}).Compose(OffsetInterval{Lower: 1, Upper: 1})
	require.Error(t, err)
	_, err = (OffsetInterval{Lower: max, Upper: max}).Widen(time.Second, 100)
	require.Error(t, err)
}

func TestClockIntersectsManyWANProbes(t *testing.T) {
	at := time.Now()
	skew := 3 * time.Second
	tracker, err := NewClockTracker(100)
	require.NoError(t, err)
	add := func(at time.Time, rtt time.Duration) {
		server := at.Add(skew + rtt/2)
		require.NoError(t, tracker.Add(ClockProbe{CallerSend: at, CallerReceive: at.Add(rtt), ServerReceive: server, ServerSend: server}))
	}
	add(at, 20*time.Millisecond)
	add(at.Add(time.Second), 70*time.Millisecond)
	mapped, err := tracker.Map(at.Add(skew+1100*time.Millisecond), OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond})
	require.NoError(t, err)
	require.Less(t, mapped.Latest.Sub(mapped.Earliest), 23*time.Millisecond, "older 20ms probe beats nearest 70ms probe, including drift and host error")
	require.True(t, mapped.Earliest.Before(at.Add(1100*time.Millisecond)))
	require.True(t, mapped.Latest.After(at.Add(1100*time.Millisecond)))
}
