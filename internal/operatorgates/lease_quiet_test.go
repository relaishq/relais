package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryPreflightFaultQuietGap(t *testing.T) {
	for _, op := range []string{"move", "drain", "worker-down"} {
		t.Run(op, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, _ := leaseGuardFixture(t, c)
			g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
			r.BeforeMutation = func(context.Context) error {
				require.GreaterOrEqual(t, c.Elapsed(), 2000340*time.Microsecond, "MUTATION: lease-fault-quiet-gap")
				return fault("stop-after-quiet")
			}
			slot := "shared"
			if op == "worker-down" {
				slot = "w1"
			}
			_, err := r.StepWithLeases(context.Background(), op, slot, time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
			require.EqualError(t, err, "stop-after-quiet")
		})
	}
}

func TestFaultQuietReacquiresHeadroom(t *testing.T) {
	for _, mode := range []string{"fresh-reference", "stale-reference", "unknown", "bound", "oversleep"} {
		t.Run(mode, func(t *testing.T) {
			c := &leaseTestClock{tick: time.Second}
			g, read := leaseGuardFixture(t, c)
			// 0.924s remains, matching the shortest observed runner headroom.
			for slot, sample := range read.Evidence.Clocks {
				o := *sample.Lease
				o.ReferenceAgeNS = 4026000000
				o.MeasuredUnixNS = "1793930004026000000"
				sample.Lease = &o
				read.Evidence.Clocks[slot] = sample
			}
			require.NoError(t, g.accept(read))
			require.NoError(t, g.Check())
			if mode == "bound" {
				// Retain normal expiry semantics; only the projected bound is tight.
				g, read = leaseGuardFixture(t, c)
				g.bounds.FrontendControlBoundNS = 2200
			}
			calls := 0
			start := c.Elapsed()
			lastDone := c.Elapsed()
			g.pause = func(_ context.Context, d time.Duration) error {
				if mode == "oversleep" && d > time.Second && calls == 1 {
					d += 5 * time.Second
				}
				c.advance(d)
				return nil
			}
			g.read = func(ctx context.Context, end time.Duration) (LeaseRead, error) {
				calls++
				if calls == 1 {
					require.Equal(t, start+100*time.Millisecond, c.Elapsed(), "MUTATION: lease-fault-short-headroom")
				}
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 1800*time.Millisecond)
				if mode == "unknown" {
					return LeaseRead{}, fault("command-result-unknown")
				}
				sent := c.Elapsed()
				c.advance(400 * time.Millisecond)
				lastDone = c.Elapsed()
				for slot, sample := range read.Evidence.Clocks {
					o := *sample.Lease
					if mode != "stale-reference" {
						o.ReferenceUnixNS = fmt.Sprint(int64(1793930010000000000) + int64(calls)*int64(time.Second))
						o.ReferenceAgeNS = 100000000
						o.MeasuredUnixNS = fmt.Sprint(int64(1793930010100000000) + int64(calls)*int64(time.Second))
						if mode == "bound" {
							o.ErrorBoundNS = 100
						}
					}
					sample.Lease = &o
					read.Evidence.Clocks[slot], read.Sent[slot] = sample, sent
				}
				return read, nil
			}
			var timing FaultQuietTiming
			err := g.prepareFault(context.Background(), start+10*time.Second, 1100*time.Millisecond, &timing)
			switch mode {
			case "stale-reference":
				require.EqualError(t, err, "clock-lease-expired", "MUTATION: lease-fault-short-headroom")
				require.LessOrEqual(t, c.Elapsed(), start+10*time.Second)
			case "unknown":
				require.EqualError(t, err, "command-result-unknown")
				require.Equal(t, 1, calls)
			default:
				require.NoError(t, err, "MUTATION: lease-fault-gap-restart")
				require.Positive(t, calls, "MUTATION: lease-fault-short-headroom")
				require.GreaterOrEqual(t, c.Elapsed()-lastDone, 1100*time.Millisecond, "MUTATION: lease-fault-gap-restart")
				if mode == "oversleep" {
					require.Equal(t, 2, calls, "MUTATION: lease-fault-gap-restart")
				}
			}
		})
	}
}

func TestFaultQuietRefusesWithoutBudget(t *testing.T) {
	for _, mode := range []string{"gap", "attempt", "unknown", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, read := leaseGuardFixture(t, c)
			g.pause = func(ctx context.Context, d time.Duration) error { c.advance(d); return ctx.Err() }
			calls := 0
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				r.Session.TransportLost()
				return LeaseRead{}, fault("command-result-unknown")
			}
			r.BeforeMutation = func(context.Context) error { t.Fatal("refused fault reached mutation hook"); return nil }
			end := c.Elapsed() + 30500*time.Millisecond
			if mode != "gap" {
				for _, sample := range read.Evidence.Clocks {
					sample.Lease.ReferenceAgeNS = 4026000000
					sample.Lease.MeasuredUnixNS = "1793930004026000000"
				}
				require.NoError(t, g.accept(read))
				end = c.Elapsed() + 31800*time.Millisecond
			}
			if mode == "unknown" {
				end = time.Minute
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
				var timing FaultQuietTiming
				require.ErrorIs(t, g.prepareFault(ctx, time.Minute, r.FaultQuietGap, &timing), context.Canceled)
				return
			}
			_, err := r.StepWithLeases(ctx, "move", "shared", end, ProcessExtra{}, LeaseStep{Guard: g})
			if mode == "unknown" {
				require.EqualError(t, err, "command-result-unknown")
				require.Equal(t, 1, calls)
				require.True(t, r.Session.Latched())
			} else {
				require.Error(t, err)
				require.Zero(t, calls)
				require.False(t, r.Session.Latched())
			}
		})
	}
}

func TestFaultQuietKeepsOriginalRefreshDeadline(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, read := leaseGuardFixture(t, c)
	c.advance(5 * time.Second)
	start := c.Elapsed()
	g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
	calls := 0
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		calls++
		sent := c.Elapsed()
		c.advance(1700 * time.Millisecond)
		for slot, sample := range read.Evidence.Clocks {
			o := *sample.Lease
			if calls == 5 {
				o.ReferenceUnixNS, o.MeasuredUnixNS = "1793930010000000000", "1793930010100000000"
			}
			sample.Lease = &o
			read.Evidence.Clocks[slot], read.Sent[slot] = sample, sent
		}
		return read, nil
	}
	require.NoError(t, g.Refresh(context.Background(), time.Minute))
	require.Equal(t, 5, calls)
	var timing FaultQuietTiming
	err := g.prepareFault(context.Background(), time.Minute, 1100*time.Millisecond, &timing)
	require.EqualError(t, err, "clock-lease-expired", "MUTATION: lease-fault-refresh-deadline")
	require.LessOrEqual(t, c.Elapsed(), start+10*time.Second)
	require.Equal(t, 5, calls)
}

func TestCallerQuietGapBoundsMapping(t *testing.T) {
	base := time.Unix(1793930000, 0)
	burst := operatorcaller.Burst{Probes: []operatorcaller.Probe{{CallerSend: base, CallerReceive: base.Add(10 * time.Millisecond), ServerReceive: base.Add(time.Hour), ServerSend: base.Add(time.Hour)}}}
	bounds := clocks.Bounds{FrontendControlBoundNS: 120000, FrontendWorkerBoundNS: 170000}
	gap, err := callerQuietGap(burst, 20*time.Minute, bounds)
	require.NoError(t, err)
	require.Equal(t, 1010340*time.Microsecond, gap, "MUTATION: lease-fault-mapping-margin")
	bounds.DriftPPM = 100
	gap, err = callerQuietGap(burst, 20*time.Minute, bounds)
	require.NoError(t, err)
	// 240ms of two-sided drift over 20 minutes, 10ms residual RTT,
	// 340us host interval, plus outward rate/distance rounding.
	require.Greater(t, gap, 1250*time.Millisecond, "MUTATION: lease-fault-mapping-margin")
	require.Less(t, gap, 1251*time.Millisecond)
	_, err = callerQuietGap(operatorcaller.Burst{}, time.Minute, bounds)
	require.EqualError(t, err, "clock-lease-missing")
}

func TestOrdinaryFaultQuietCancellationTiming(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	g.pause = func(context.Context, time.Duration) error {
		c.advance(400 * time.Millisecond)
		return context.Canceled
	}
	var timing FaultQuietTiming
	err := g.prepareFault(context.Background(), time.Minute, 1100*time.Millisecond, &timing)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int64(400*time.Millisecond), timing.WaitedNS)
	require.Equal(t, int64(time.Second), timing.LastReadDoneNS)
	require.Zero(t, timing.BeforeSendNS)
}

func TestFaultQuietSendRechecksAndRestorationBypasses(t *testing.T) {
	for _, mode := range []string{"read-after-gap", "expired-after-gap", "rollback", "rollback-drain", "recovery", "unfreeze"} {
		t.Run(mode, func(t *testing.T) {
			r, peer, _ := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, read := leaseGuardFixture(t, c)
			g.pause = func(_ context.Context, d time.Duration) error { c.advance(d); return nil }
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				t.Fatal("unexpected read")
				return LeaseRead{}, nil
			}
			r.BeforeMutation = func(context.Context) error {
				if mode == "read-after-gap" {
					require.NoError(t, g.accept(read))
				} else {
					c.advance(5 * time.Second)
				}
				return nil
			}
			op := "move"
			restore := mode == "rollback" || mode == "rollback-drain" || mode == "recovery" || mode == "unfreeze"
			if restore {
				r.BeforeMutation = func(context.Context) error { t.Fatal("restoration armed"); return nil }
				g.pause = func(context.Context, time.Duration) error { t.Fatal("restoration waited for quiet gap"); return nil }
				g.invalid = true
				op = "replace"
				if mode == "rollback" || mode == "rollback-drain" {
					r.Config.Operation, r.Config.Target = "rollback", "from"
				}
				if mode == "rollback-drain" {
					op = "drain"
					previous := peer.respond
					peer.respond = func(q operator.Request) operator.Reply {
						if q.Action != "drain" {
							return previous(q)
						}
						return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": c.Wall(), "ended": c.Wall(), "data": controlplane.DrainResult{Moves: []controlplane.MoveResult{}}}))
					}
				}
				if mode == "unfreeze" {
					op = "unfreeze"
				}
			}
			_, err := r.StepWithLeases(context.Background(), op, "shared", time.Minute, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g, Recovery: mode == "recovery"})
			if restore {
				require.NoError(t, err, "MUTATION: lease-fault-restore-gap")
				require.Equal(t, time.Second, c.Elapsed(), "MUTATION: lease-fault-restore-gap")
			} else {
				require.EqualError(t, err, "clock-lease-expired", "MUTATION: lease-fault-send-check")
			}
			require.False(t, r.Session.Latched())
		})
	}
}
