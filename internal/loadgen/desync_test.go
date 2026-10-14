package loadgen

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spread asserts every value is in [0, period) and each tenth of the period
// holds at least a third of its expected share.
func spread(t *testing.T, name string, values []time.Duration, period time.Duration) {
	t.Helper()
	var bins [10]int
	for i, v := range values {
		require.GreaterOrEqual(t, v, time.Duration(0), "%s %d", name, i)
		require.Less(t, v, period, "%s %d", name, i)
		bins[int(v*10/period)]++
	}
	for i, n := range bins {
		assert.GreaterOrEqual(t, n, len(values)/30, "%s bin %d of %v", name, i, bins)
	}
}

func TestDesyncOffsetsAreSeededAndSpreadAcrossOneSecond(t *testing.T) {
	const calls = 1000
	d := Desync{Seed: 41, Process: 3}
	media := make([]time.Duration, calls)
	maintenance := make([]time.Duration, calls)
	for i := range calls {
		media[i], maintenance[i] = d.MediaStart(i), d.Maintenance(i)
		again := Desync{Seed: 41, Process: 3}
		require.Equal(t, media[i], again.MediaStart(i), "a seed reproduces each offset")
		require.Equal(t, maintenance[i], again.Maintenance(i))
	}
	spread(t, "media start", media, time.Second)
	spread(t, "maintenance", maintenance, time.Second)

	// The run seed and each part of the call's identity change the offsets.
	for name, other := range map[string]Desync{"seed": {Seed: 42, Process: 3}, "process": {Seed: 41, Process: 4}} {
		same := 0
		for i := range calls {
			if other.MediaStart(i) == media[i] {
				same++
			}
		}
		assert.Zero(t, same, "%s changes the offsets", name)
	}
	assert.NotEqual(t, d.MediaStart(0), d.MediaStart(1), "the call index changes the offset")
}

func TestDesyncPhasesDifferAcrossCallsAndProcesses(t *testing.T) {
	// A small run: four processes of two calls, as an eight-caller soak lays out.
	const near = 5 * time.Millisecond
	var maintenance, samplers []time.Duration
	for p := range 4 {
		d := Desync{Seed: 41, Process: p}
		samplers = append(samplers, d.SamplePhase(time.Second))
		for i := range 2 {
			maintenance = append(maintenance, d.Maintenance(i))
			assert.NotEqual(t, d.MediaStart(i), d.Maintenance(i), "media start and maintenance are independent draws")
		}
	}
	for _, values := range [][]time.Duration{maintenance, samplers} {
		for i := range values {
			for j := range i {
				assert.Greater(t, (values[i] - values[j]).Abs(), near, "phases %d and %d coincide: %v", i, j, values)
			}
		}
	}
}

func TestDesyncSamplePhaseStaysWithinTheInterval(t *testing.T) {
	for _, every := range []time.Duration{100 * time.Millisecond, time.Second, 7 * time.Second} {
		phases := make([]time.Duration, 1000)
		for p := range phases {
			phases[p] = Desync{Seed: 41, Process: p}.SamplePhase(every)
		}
		spread(t, "sample phase "+every.String(), phases, every)
	}
	assert.Zero(t, Desync{Seed: 41}.SamplePhase(0))
}

func TestSendAfterDelaysButKeepsTheFullDuration(t *testing.T) {
	const offset, duration = 150 * time.Millisecond, 7 * time.Second
	var sentFor time.Duration
	var sentAt time.Time
	send := func(_ context.Context, d time.Duration) error {
		sentAt, sentFor = time.Now(), d
		return nil
	}
	started := time.Now()
	require.NoError(t, sendAfter(context.Background(), offset, duration, send))
	assert.GreaterOrEqual(t, sentAt.Sub(started), offset, "media waits for its offset")
	assert.Equal(t, duration, sentFor, "the offset never shortens the media duration")

	sentFor = 0
	started = time.Now()
	require.NoError(t, sendAfter(context.Background(), 0, duration, send))
	assert.Less(t, sentAt.Sub(started), 100*time.Millisecond, "a zero offset starts at once")
	assert.Equal(t, duration, sentFor)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sentFor = 0
	require.ErrorIs(t, sendAfter(ctx, time.Hour, duration, send), context.Canceled)
	assert.Zero(t, sentFor, "a cancelled wait sends nothing")
}
