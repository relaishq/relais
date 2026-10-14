package loadgen

import (
	"context"
	"time"
)

// Desync spreads a generator's per-call and per-process periodic work over
// time (#85). Real callers start independently, so their media ticks,
// keyframes and housekeeping never line up. Synthetic callers that all start
// media on one instant and run one-second work on one phase put a burst on
// the server once per second, and the run then measures that burst instead of
// Relais.
//
// Every value is a pure function of the run seed, the generator process index
// and the call index, so a seed reproduces a run's offsets exactly.
type Desync struct {
	Seed    int64
	Process int
}

// desyncPeriod covers one cycle of the caller's one-second periodic work:
// the harness VP8 fixture's keyframe interval and recording maintenance.
const desyncPeriod = time.Second

// Independent draws for one identity. Changing these values changes every
// seeded offset.
const (
	desyncMediaStart uint64 = iota + 1
	desyncMaintenance
	desyncSampler
)

// MediaStart is how long after the common start time the call begins
// sending media, in [0, 1s). The call still sends for the full duration from
// its own start, so its keyframes and frame ticks keep this phase.
func (d Desync) MediaStart(call int) time.Duration {
	return d.draw(call, desyncMediaStart, desyncPeriod)
}

// Maintenance is the call's recording maintenance phase in [0, 1s); see
// callharness.RecordingOptions.MaintenancePhase.
func (d Desync) Maintenance(call int) time.Duration {
	return d.draw(call, desyncMaintenance, desyncPeriod)
}

// SamplePhase is this process's resource-sampling phase in [0, every). The
// first sample covers every plus the phase; later samples keep the interval.
func (d Desync) SamplePhase(every time.Duration) time.Duration {
	return d.draw(-1, desyncSampler, every)
}

func (d Desync) draw(call int, stream uint64, period time.Duration) time.Duration {
	if period <= 0 {
		return 0
	}
	// Two's-complement bit patterns: the conversions keep every input distinct.
	h := splitmix64(uint64(d.Seed))
	h = splitmix64(h ^ uint64(int64(d.Process)))
	h = splitmix64(h ^ uint64(int64(call)))
	h = splitmix64(h ^ stream)
	// The modulo bias over 2^64 is below one part in 10^9 for a one-second period.
	return time.Duration(h % uint64(period))
}

// splitmix64 is the SplitMix64 step and finalizer: a fixed integer mix, so
// offsets do not depend on the Go release's random sources.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// sendAfter waits offset, then sends media for the whole duration. The offset
// delays the call's media; it never shortens it. Cancellation during the wait
// returns the context's error without sending.
func sendAfter(ctx context.Context, offset, duration time.Duration, send func(context.Context, time.Duration) error) error {
	if offset > 0 {
		timer := time.NewTimer(offset)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return send(ctx, duration)
}
