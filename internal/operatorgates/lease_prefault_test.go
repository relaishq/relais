package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreFaultExpiredEntry(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, read := leaseGuardFixture(t, c)
			// Refresh sees 0.6 ms of valid lifetime. Script preparation consumes 1 ms.
			c.advance(4849400 * time.Microsecond)
			require.NoError(t, g.Refresh(context.Background(), time.Minute))
			calls := 0
			c.advance(time.Millisecond)
			start := c.Elapsed()
			end := start + 10*time.Second
			if mode == "deadline" {
				end = start + 1800*time.Millisecond
			}
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				sent := c.Elapsed()
				c.advance(400 * time.Millisecond)
				for slot, sample := range read.Evidence.Clocks {
					o := *sample.Lease
					if mode == "fresh" {
						o.ReferenceUnixNS, o.MeasuredUnixNS, o.ReferenceAgeNS = "1793930010000000000", "1793930010100000000", 100000000
					}
					sample.Lease = &o
					read.Evidence.Clocks[slot], read.Sent[slot] = sample, sent
				}
				return read, nil
			}
			var timing FaultQuietTiming
			err := g.prepareFault(context.Background(), end, 1100*time.Millisecond, &timing)
			if mode == "fresh" {
				require.NoError(t, err, "MUTATION: prefault-expired-entry")
				require.Equal(t, 1, calls)
				require.Equal(t, start+1600*time.Millisecond, c.Elapsed())
			} else {
				require.EqualError(t, err, "clock-lease-expired")
				if mode == "deadline" {
					require.Zero(t, calls)
					require.Equal(t, start+100*time.Millisecond, c.Elapsed())
				} else {
					require.Equal(t, 17, calls)
					require.Equal(t, start+8600*time.Millisecond, c.Elapsed())
				}
			}
			require.Equal(t, int64(1100*time.Millisecond), timing.RequiredGapNS, "MUTATION: prefault-expired-entry")
		})
	}
}

func TestPreFaultHeadroomCharacterization(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, read := leaseGuardFixture(t, c)
			// Only w2 lacks the projected 1.1 s quiet gap.
			read.Evidence.Clocks["w2"].Lease.ReferenceAgeNS = 4026000000
			read.Evidence.Clocks["w2"].Lease.MeasuredUnixNS = "1793930004026000000"
			require.NoError(t, g.accept(read))
			require.NoError(t, g.Check())
			start := c.Elapsed()
			end := start + 10*time.Second
			if mode == "deadline" {
				end = start + 1800*time.Millisecond
			}
			calls := 0
			g.read = func(ctx context.Context, until time.Duration) (LeaseRead, error) {
				calls++
				require.Equal(t, end, until)
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 1800*time.Millisecond)
				sent := c.Elapsed()
				c.advance(400 * time.Millisecond)
				for slot, sample := range read.Evidence.Clocks {
					o := *sample.Lease
					if mode == "fresh" {
						o.ReferenceUnixNS, o.MeasuredUnixNS, o.ReferenceAgeNS = "1793930010000000000", "1793930010100000000", 100000000
					}
					sample.Lease = &o
					read.Evidence.Clocks[slot], read.Sent[slot] = sample, sent
				}
				return read, nil
			}
			var timing FaultQuietTiming
			err := g.prepareFault(context.Background(), end, 1100*time.Millisecond, &timing)
			switch mode {
			case "fresh":
				require.NoError(t, err)
				require.Equal(t, 1, calls)
				require.Equal(t, start+1600*time.Millisecond, c.Elapsed())
			case "stale":
				require.EqualError(t, err, "clock-lease-expired")
				require.Equal(t, 17, calls)
				require.Equal(t, start+8600*time.Millisecond, c.Elapsed())
			case "deadline":
				require.EqualError(t, err, "clock-lease-expired")
				require.Zero(t, calls)
				require.Equal(t, start+100*time.Millisecond, c.Elapsed())
			}
		})
	}
}

func TestPreFaultSchedulingMargin(t *testing.T) {
	for _, mode := range []string{"projected", "gap-already-done", "oversleep", "enough-headroom"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, read := leaseGuardFixture(t, c)
			const gap = 1100 * time.Millisecond
			switch mode {
			case "projected", "enough-headroom":
				// w2 expires just after the gap, inside the 10 ms scheduling reserve.
				age := int64(3849500 * time.Microsecond)
				if mode == "enough-headroom" {
					age -= int64(20 * time.Millisecond)
				}
				read.Evidence.Clocks["w2"].Lease.ReferenceAgeNS = age
				read.Evidence.Clocks["w2"].Lease.MeasuredUnixNS = fmt.Sprint(int64(1793930000000000000) + age)
				require.NoError(t, g.accept(read))
			case "gap-already-done":
				c.advance(4849500 * time.Microsecond)
			}
			calls := 0
			g.pause = func(_ context.Context, d time.Duration) error {
				if mode == "oversleep" && calls == 0 && d == gap {
					d += 3749500 * time.Microsecond
				}
				c.advance(d)
				return nil
			}
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				sent := c.Elapsed()
				c.advance(400 * time.Millisecond)
				for slot, sample := range read.Evidence.Clocks {
					o := *sample.Lease
					o.ReferenceUnixNS, o.MeasuredUnixNS, o.ReferenceAgeNS = "1793930010000000000", "1793930010100000000", 100000000
					sample.Lease = &o
					read.Evidence.Clocks[slot], read.Sent[slot] = sample, sent
				}
				return read, nil
			}
			var timing FaultQuietTiming
			require.NoError(t, g.prepareFault(context.Background(), time.Minute, gap, &timing), "MUTATION: prefault-scheduling-margin")
			want := 1
			if mode == "enough-headroom" {
				want = 0
			}
			require.Equal(t, want, calls, "MUTATION: prefault-scheduling-margin")
			require.Greater(t, timing.RemainingNS, int64(10*time.Millisecond), "MUTATION: prefault-scheduling-margin")
		})
	}
}

func TestPreFaultRefusalDiagnostics(t *testing.T) {
	for _, mode := range []string{"headroom", "expired", "send-expired"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, read := leaseGuardFixture(t, c)
			read.Evidence.Clocks["w2"].Lease.ReferenceAgeNS = 4026000000
			read.Evidence.Clocks["w2"].Lease.MeasuredUnixNS = "1793930004026000000"
			require.NoError(t, g.accept(read))
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				t.Fatal("unexpected read")
				return LeaseRead{}, nil
			}
			var timing FaultQuietTiming
			remaining := int64(824 * time.Millisecond)
			if mode != "headroom" {
				c.advance(time.Second)
				remaining = 0
			}
			var err error
			if mode == "send-expired" {
				err = g.checkFaultQuiet(1100*time.Millisecond, &timing)
			} else {
				err = g.prepareFault(context.Background(), c.Elapsed()+1800*time.Millisecond, 1100*time.Millisecond, &timing)
			}
			require.EqualError(t, err, "clock-lease-expired")
			require.Equal(t, "w2", timing.LimitingSlot, "MUTATION: prefault-refusal-diagnostics")
			require.Equal(t, remaining, timing.RemainingNS, "MUTATION: prefault-refusal-diagnostics")
		})
	}
}

func TestPreFaultRuntimeRefusalDiagnostics(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	c := &leaseTestClock{tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	g, _ := leaseGuardFixture(t, c)
	r.BeforeMutation = func(context.Context) error { c.advance(5 * time.Second); return nil }
	observed := false
	r.FaultObserved = func(timing FaultQuietTiming) {
		observed = true
		require.Equal(t, "relay", timing.LimitingSlot)
		require.Zero(t, timing.RemainingNS)
	}
	_, err := r.StepWithLeases(context.Background(), "move", "shared", time.Minute, ProcessExtra{}, LeaseStep{Guard: g})
	require.EqualError(t, err, "clock-lease-expired")
	require.True(t, observed)
}
