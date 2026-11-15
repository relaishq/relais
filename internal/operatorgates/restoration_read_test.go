package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/stretchr/testify/require"
)

// The endpoint returns cursor deltas: each read consumes its tracking rows.
func restorationReadFixture(t *testing.T) (*LeaseGuard, *FakeClock, func(int64, int64, bool) LeaseRead) {
	t.Helper()
	const base int64 = 1793930000
	c := &FakeClock{Now: time.Unix(base, 0), Tick: time.Second}
	g, initial := leaseGuardFixture(t, c)
	for slot, sample := range initial.Evidence.Clocks {
		sample.TrackingLog.Rows = []clocks.TrackingRow{{UnixSeconds: base, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"}}
		initial.Evidence.Clocks[slot] = sample
	}
	require.NoError(t, g.accept(initial))
	c.Advance(200 * time.Second)
	endpoint := func(first, last int64, expired bool) LeaseRead {
		next := LeaseRead{Sent: map[string]time.Duration{}, Evidence: clocks.Evidence{Clocks: map[string]clocks.Sample{}}}
		for slot, old := range initial.Evidence.Clocks {
			o := *old.Lease
			o.MeasuredUnixNS = fmt.Sprint(c.Wall().UnixNano())
			o.ReferenceAgeNS = int64(100 * time.Millisecond)
			if expired {
				o.ReferenceAgeNS = int64(6 * time.Second)
			}
			o.ReferenceUnixNS = fmt.Sprint(c.Wall().UnixNano() - o.ReferenceAgeNS)
			rows := []clocks.TrackingRow{}
			for second := first; second <= last; second++ {
				rows = append(rows, clocks.TrackingRow{UnixSeconds: base + second, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"})
			}
			next.Evidence.Clocks[slot] = clocks.Sample{Lease: &o, TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: rows}}
			next.Sent[slot] = c.Elapsed() - 50*time.Millisecond
		}
		return next
	}
	return g, c, endpoint
}

func TestRestorationReadCharacterization(t *testing.T) {
	for _, mode := range []string{"fresh", "expired", "transport-unknown", "reserve-exhausted", "lost-rows"} {
		t.Run(mode, func(t *testing.T) {
			g, c, endpoint := restorationReadFixture(t)
			calls := 0
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				if mode == "transport-unknown" {
					return LeaseRead{}, fault("command-result-unknown")
				}
				first := int64(1)
				if mode == "lost-rows" {
					first = 100
				}
				return endpoint(first, 200, mode == "expired"), nil
			}
			end := c.Elapsed() + RestoreReserve + g.session.timing.leaseAttempt()
			if mode == "reserve-exhausted" {
				end -= time.Nanosecond
			}
			err := g.coverRestoration(context.Background(), end, c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond))
			switch mode {
			case "fresh":
				require.NoError(t, err)
			case "expired", "reserve-exhausted":
				require.EqualError(t, err, "clock-lease-expired")
			case "transport-unknown":
				require.EqualError(t, err, "command-result-unknown")
			case "lost-rows":
				require.EqualError(t, err, "clock-lease-missing")
			}
			want := 1
			if mode == "reserve-exhausted" {
				want = 0
			}
			require.Equal(t, want, calls, "characterized endpoint read count")
		})
	}
}

func TestRestorationExpiredThenFresh(t *testing.T) {
	g, c, endpoint := restorationReadFixture(t)
	g.session.timing.poll = 2 * time.Second
	began := c.Elapsed()
	start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
	calls := 0
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		calls++
		if calls == 1 {
			return endpoint(1, 200, true), nil
		}
		c.Advance(time.Second)
		return endpoint(201, 201, false), nil
	}
	err := g.coverRestoration(context.Background(), c.Elapsed()+RestoreReserve+10*time.Second, start, finish)
	require.NoError(t, err, "MUTATION: restoration-fresh-retry")
	require.Equal(t, 2, calls)
	require.Equal(t, 1200*time.Millisecond, c.Elapsed()-began, "retry spacing must follow the two-second poll")
}

func TestRestorationRetryRetainsHistory(t *testing.T) {
	for _, mode := range []string{"gap", "bound", "empty-tail"} {
		t.Run(mode, func(t *testing.T) {
			g, c, endpoint := restorationReadFixture(t)
			start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
			calls := 0
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				if calls > 1 {
					c.Advance(time.Second)
					if mode == "empty-tail" {
						return endpoint(1, 0, false), nil
					}
					return endpoint(201, 201, false), nil
				}
				first := int64(1)
				if mode == "gap" {
					first = 100
				}
				read := endpoint(first, 200, true)
				for slot, sample := range read.Evidence.Clocks {
					if mode == "bound" {
						sample.TrackingLog.Rows[99].MaxErrorSeconds = .001
						sample.TrackingLog.Rows[99].MaxErrorDecimal = "1.000e-03"
					}
					read.Evidence.Clocks[slot] = sample
				}
				return read, nil
			}
			err := g.coverRestoration(context.Background(), c.Elapsed()+RestoreReserve+10*time.Second, start, finish)
			if mode == "empty-tail" {
				require.NoError(t, err, "consumed rows must survive an empty fresh delta")
			} else {
				want := "clock-lease-missing"
				if mode == "bound" {
					want = "clock-lease-bound"
				}
				require.EqualError(t, err, want, "MUTATION: restoration-whole-history")
			}
			require.Equal(t, 2, calls)
		})
	}
}

func TestRestorationRetryBudget(t *testing.T) {
	for _, mode := range []string{"persistent-expiry", "transport-unknown", "read-over-cutoff", "pause-over-cutoff", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			g, c, endpoint := restorationReadFixture(t)
			g.session.timing.read = 300 * time.Millisecond
			now := c.Elapsed()
			start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
			// A 1.4 s attempt fits at 0, 0.1, 0.2 s before this earlier cutoff.
			cutoff := now + 1600*time.Millisecond
			calls, pauses := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g.read = func(_ context.Context, end time.Duration) (LeaseRead, error) {
				calls++
				require.Equal(t, cutoff, end, "MUTATION: restoration-cutoff")
				require.LessOrEqual(t, c.Elapsed()+g.session.timing.leaseAttempt(), cutoff, "MUTATION: restoration-cutoff")
				if mode == "read-over-cutoff" {
					c.Advance(3 * time.Second)
					return endpoint(1, 200, false), nil
				}
				if calls > 1 && mode == "transport-unknown" {
					return LeaseRead{}, fault("command-result-unknown")
				}
				return endpoint(1, 200, true), nil
			}
			g.pause = func(_ context.Context, d time.Duration) error {
				pauses++
				require.Equal(t, 100*time.Millisecond, d, "spacing follows the configured poll period")
				c.Advance(d)
				if mode == "pause-over-cutoff" {
					c.Advance(3 * time.Second)
				}
				if mode == "cancelled" {
					cancel()
					return context.Canceled
				}
				return nil
			}
			err := g.coverRestoration(ctx, cutoff+RestoreReserve, start, finish)
			want, stage, reads := "clock-lease-expired", "reserve-exhausted", 3
			switch mode {
			case "transport-unknown":
				want, stage, reads = "command-result-unknown", "transport-unknown", 2
			case "read-over-cutoff", "pause-over-cutoff":
				reads = 1
			case "cancelled":
				require.ErrorIs(t, err, context.Canceled)
				want, stage, reads = "context canceled", "retry-wait", 1
			}
			require.EqualError(t, err, want, "MUTATION: restoration-cutoff")
			require.Equal(t, reads, calls, "MUTATION: restoration-cutoff")
			var failure *restorationError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, stage, failure.stage, "MUTATION: restoration-stage")
			if mode == "persistent-expiry" {
				require.Equal(t, 2, pauses)
			}
		})
	}
}

func TestRestorationRetryPollCap(t *testing.T) {
	for _, mode := range []string{"persistent", "late-fresh"} {
		t.Run(mode, func(t *testing.T) {
			g, c, endpoint := restorationReadFixture(t)
			g.session.timing.read = 50 * time.Millisecond
			began := c.Elapsed()
			start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
			cap := began + 2100*time.Millisecond // First expired reply at +100 ms, then 2P.
			calls := 0
			g.read = func(_ context.Context, end time.Duration) (LeaseRead, error) {
				calls++
				if calls > 1 {
					require.Equal(t, cap, end, "MUTATION: restoration-poll-cap")
				}
				c.Advance(100 * time.Millisecond)
				if calls > 1 && mode == "late-fresh" {
					c.Advance(2 * time.Second)
					return endpoint(201, 202, false), nil
				}
				return endpoint(1, 200, true), nil
			}
			err := g.coverRestoration(context.Background(), began+RestoreReserve+10*time.Second, start, finish)
			require.EqualError(t, err, "clock-lease-expired", "MUTATION: restoration-poll-cap")
			var failure *restorationError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "refresh-window-exhausted", failure.stage, "MUTATION: restoration-poll-cap")
			reads := 9
			if mode == "late-fresh" {
				reads = 2
			}
			require.Equal(t, reads, calls, "MUTATION: restoration-poll-cap")
		})
	}
}

func TestRestorationEmptyRetryKeepsNextBasis(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(fmt.Sprint("gap=", gap), func(t *testing.T) {
			g, c, endpoint := restorationReadFixture(t)
			calls := 0
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				switch calls {
				case 1:
					return endpoint(1, 200, true), nil
				case 2:
					c.Advance(time.Second)
					return endpoint(1, 0, false), nil
				default:
					if gap {
						return endpoint(300, 301, false), nil
					}
					return endpoint(201, 207, false), nil
				}
			}
			cover := func() error {
				return g.coverRestoration(context.Background(), c.Elapsed()+RestoreReserve+10*time.Second, c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond))
			}
			require.NoError(t, cover())
			delay := 6 * time.Second
			if gap {
				delay = 100 * time.Second
			}
			c.Advance(delay)
			err := cover()
			if gap {
				require.EqualError(t, err, "clock-lease-missing", "MUTATION: restoration-next-basis")
			} else {
				require.NoError(t, err, "MUTATION: restoration-next-basis")
			}
			require.Equal(t, 3, calls, "MUTATION: restoration-next-basis")
		})
	}
}

func TestRestorationRetryLeavesProofHeadroom(t *testing.T) {
	g, c, endpoint := restorationReadFixture(t)
	start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
	calls := 0
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		calls++
		if calls == 1 {
			return endpoint(1, 200, true), nil
		}
		c.Advance(time.Second)
		return endpoint(201, 201, false), nil
	}
	g.restorationReadObserved = func(LeaseRead, time.Duration, time.Duration) {
		if calls == 1 {
			c.Advance(25 * time.Millisecond)
		}
	}
	err := g.coverRestoration(context.Background(), c.Elapsed()+RestoreReserve+10*time.Second, start, finish)
	require.NoError(t, err, "MUTATION: restoration-retry-headroom")
	require.Equal(t, 2, calls, "MUTATION: restoration-retry-headroom")
}
