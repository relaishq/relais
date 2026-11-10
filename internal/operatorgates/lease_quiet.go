package operatorgates

import (
	"context"
	"math"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/callharness"
)

// callerQuietGap protects all ordinary preflight host reads before planned faults.
// Restoration is exempt, including stable-from rollback moves and their
// unchanged post-operation coverage reads. Bound the mapped start interval using one
// registration probe already retained by the caller. Later probe intersections
// can only narrow it. horizon covers caller launch through its original end,
// so no gate/host wall-clock synchronization or future probe is assumed.
func callerQuietGap(burst operatorcaller.Burst, horizon time.Duration, bounds clocks.Bounds) (time.Duration, error) {
	width := time.Duration(math.MaxInt64)
	host := time.Duration(max(bounds.FrontendControlBoundNS, bounds.FrontendWorkerBoundNS))
	for _, p := range burst.Probes {
		tracker, err := callharness.NewClockTracker(bounds.DriftPPM)
		if err != nil {
			return 0, fault("clock-config-differ")
		}
		probe := callharness.ClockProbe{CallerSend: p.CallerSend, CallerReceive: p.CallerReceive, ServerReceive: p.ServerReceive, ServerSend: p.ServerSend}
		if err = tracker.Add(probe); err != nil {
			return 0, fault("clock-config-differ")
		}
		mapped, err := tracker.Map(p.ServerSend.Add(horizon), callharness.OffsetInterval{Lower: -host, Upper: host})
		if err != nil {
			return 0, fault("clock-config-differ")
		}
		width = min(width, mapped.Latest.Sub(mapped.Earliest))
	}
	if width == time.Duration(math.MaxInt64) {
		return 0, fault("clock-lease-missing")
	}
	// Convert the baseline second outward at the same stated relative-rate bound
	// used by mapping. The full interval width, not its half-width, covers a true
	// start at either extreme. No additional empirical sleep margin is hidden here.
	baseline := time.Duration(math.Ceil(float64(time.Second) / (1 - bounds.DriftPPM/1e6)))
	return baseline + width, nil
}

type FaultQuietTiming struct {
	Action         string `json:"action"`
	Slot           string `json:"slot"`
	LastReadDoneNS int64  `json:"lastReadDoneNs"` // All host reads; retain the existing diagnostic key.
	RequiredGapNS  int64  `json:"requiredGapNs"`
	WaitedNS       int64  `json:"waitedNs"`
	BeforeSendNS   int64  `json:"beforeSendNs"`
	RemainingNS    int64  `json:"remainingNs"`
	Reacquires     int    `json:"reacquires"`
}

// prepareFault separates ordinary preflight reads from planned fault baselines;
// it is never called for restoration. It changes neither expiry nor Refresh policy. A
// projected expiry/bound failure needs an explicit read even if Check is valid
// now. All such reads and restarted gaps share one original 10P/end budget.
func (g *LeaseGuard) prepareFault(ctx context.Context, end, gap time.Duration, timing *FaultQuietTiming) error {
	g.refresh.Lock()
	defer g.refresh.Unlock()
	defer func() {
		timing.LastReadDoneNS = int64(g.session.hostReadCompleted())
	}()
	if gap < time.Second {
		return fault("clock-lease-missing")
	}
	if err := g.Check(); err != nil {
		return err
	}
	until := end
	if g.refreshUntil > 0 {
		until = min(until, g.refreshUntil)
	}
	g.mu.Lock()
	for _, o := range g.observations {
		budget, err := clocks.LeaseRefreshBudget(o)
		if err != nil {
			g.mu.Unlock()
			return err
		}
		until = min(until, g.clock.Elapsed()+budget)
	}
	g.mu.Unlock()
	timing.RequiredGapNS = int64(gap)
	for ctx.Err() == nil {
		done := g.session.hostReadCompleted()
		timing.LastReadDoneNS = int64(done)
		now := g.clock.Elapsed()
		ready := max(now, done+gap)
		if ready >= until {
			return fault("clock-lease-expired")
		}
		err := g.checkAt(ready)
		if err == nil {
			if ready > now {
				err = g.pause(ctx, ready-now)
				timing.WaitedNS += int64(g.clock.Elapsed() - now)
				if err != nil {
					return err
				}
			}
			if err = g.checkFaultQuiet(gap, timing); err == nil {
				return nil
			}
			if code(err) != "clock-lease-expired" && code(err) != "clock-lease-bound" {
				return err
			}
			continue
		}
		if code(err) != "clock-lease-expired" && code(err) != "clock-lease-bound" {
			return err
		}
		if err = g.pause(ctx, min(100*time.Millisecond, until-now)); err != nil {
			return err
		}
		if until-g.clock.Elapsed() < leaseReadAttempt {
			return fault("clock-lease-expired")
		}
		wait, cancel := context.WithTimeout(ctx, until-g.clock.Elapsed())
		read, err := g.readAttempt(wait, until)
		cancel()
		g.mu.Lock()
		if g.reading && len(read.Sent) > 0 {
			timing.Reacquires++
		}
		g.mu.Unlock()
		accepted := g.accept(read)
		if err != nil {
			return err
		} // Unknown transport is never retried.
		if accepted != nil && code(accepted) != "clock-lease-missing" {
			return accepted
		}
	}
	return ctx.Err()
}

// This remains pure while holding the send lock: no refresh, sleep or replay.
func (g *LeaseGuard) checkFaultQuiet(gap time.Duration, timing *FaultQuietTiming) error {
	if err := g.Check(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Elapsed()
	done := g.session.hostReadCompleted()
	if g.reading || now-done < gap {
		return fault("clock-lease-expired")
	}
	timing.LastReadDoneNS = int64(done)
	timing.RemainingNS = math.MaxInt64
	for _, l := range g.leases {
		v, err := l.At(now)
		if err != nil {
			return err
		}
		timing.RemainingNS = min(timing.RemainingNS, v.RemainingNS)
	}
	return nil
}
