package operatorgates

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type LeaseRead struct {
	Evidence clocks.Evidence
	Sent     map[string]time.Duration
}

// LeaseGuard protects bound fault operations. Its reader timestamps request-send
// after the exchange lock.
type LeaseGuard struct {
	mu, refresh               sync.Mutex
	clock                     Clock
	read                      func(context.Context, time.Duration) (LeaseRead, error)
	pause                     func(context.Context, time.Duration) error
	leases                    map[string]clocks.Lease
	samples                   map[string]clocks.Sample
	observations              map[string]clocks.LeaseObservation
	bounds                    clocks.Bounds
	frontend, control, growth string
	workerBound               int64
	frozenEnd                 int64
	invalid                   bool
	historyComplete           bool
	lastReadDone              time.Duration
	reading                   bool
	refreshUntil              time.Duration
	restorationReadObserved   func(LeaseRead, time.Duration, time.Duration)
}

func NewLeaseGuard(e clocks.CoverageEvidence, session *Session, read func(context.Context, time.Duration) (LeaseRead, error), pause func(context.Context, time.Duration) error) (*LeaseGuard, error) {
	if e.Binding == nil || e.ValidateBinding(e.ClockReceiptSHA256) != nil || session == nil || session.clock == nil || read == nil || pause == nil {
		return nil, fault("clock-config-differ")
	}
	r := e.Binding.Receipt
	return &LeaseGuard{clock: session.clock, read: read, pause: pause, leases: map[string]clocks.Lease{}, samples: map[string]clocks.Sample{}, observations: map[string]clocks.LeaseObservation{}, bounds: r.Bounds, frontend: r.FrontendSlot, control: r.ControlSlot, growth: r.Policy.MaxDispersionGrowthPPM, workerBound: r.Bounds.FrontendWorkerBoundNS - r.Policy.SignalReserveNS, frozenEnd: r.EndUnixSeconds}, nil
}

func (g *LeaseGuard) accept(r LeaseRead) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	defer func() { g.lastReadDone, g.reading = g.clock.Elapsed(), false }()
	// Session cursors advance even if acceptance stops before storing every slot.
	// A later historical proof must not bridge rows lost at that boundary.
	g.historyComplete = false
	g.invalid = g.invalid || clocks.HasFalsification(r.Evidence)
	for _, s := range r.Evidence.Clocks {
		if s.TrackingLog == nil {
			continue
		}
		for _, b := range s.TrackingLog.Breaks {
			if b.UnixSeconds >= g.frozenEnd {
				g.invalid = true
			}
		}
	}
	for _, slot := range inventory.Slots {
		s := r.Evidence.Clocks[slot]
		sent, ok := r.Sent[slot]
		if !ok || s.Lease == nil || s.TrackingLog == nil || !s.TrackingLog.ReadComplete {
			return fault("clock-lease-missing")
		}
		for _, row := range s.TrackingLog.Rows {
			if row.UnixSeconds >= g.frozenEnd && row.LeapStatus != "N" {
				g.invalid = true
			}
		}
		l, err := g.leases[slot].Observe(*s.Lease, sent, g.clock.Elapsed())
		if err != nil {
			return err
		}
		g.leases[slot], g.observations[slot] = l, *s.Lease
		g.samples[slot] = s
	}
	g.historyComplete = true
	return nil
}

// Check is pure with respect to host I/O, so it can run after arm and while
// holding the send lock. It must never try to refresh from that lock.
func (g *LeaseGuard) Check() error {
	return g.checkAt(g.clock.Elapsed())
}

func (g *LeaseGuard) checkAt(at time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.invalid {
		return fault("clock-config-differ")
	}
	values := map[string]int64{}
	declared, _ := new(big.Rat).SetString(g.growth)
	for _, slot := range inventory.Slots {
		v, err := g.leases[slot].At(at)
		if err != nil {
			return err
		}
		if v.Expired {
			return fault("clock-lease-expired")
		}
		measured, ok := new(big.Rat).SetString(v.GrowthPPM)
		if !ok || measured.Cmp(declared) > 0 {
			return fault("clock-config-differ")
		}
		values[slot] = v.BoundNS
	}
	f := values[g.frontend]
	if f > g.bounds.FrontendControlBoundNS || values[g.control] > g.bounds.FrontendControlBoundNS-f || f > g.workerBound || max(values["w1"], values["w2"]) > g.workerBound-f {
		return fault("clock-lease-bound")
	}
	return nil
}

func (g *LeaseGuard) workerError() (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Elapsed()
	values := map[string]int64{}
	for _, slot := range []string{g.frontend, "w1", "w2"} {
		v, err := g.leases[slot].At(now)
		if err != nil {
			return 0, err
		}
		values[slot] = v.BoundNS
	}
	w := max(values["w1"], values["w2"])
	if values[g.frontend] > math.MaxInt64-w {
		return 0, fault("clock-lease-bound")
	}
	return values[g.frontend] + w, nil
}

func (g *LeaseGuard) Refresh(ctx context.Context, end time.Duration) error {
	g.refresh.Lock()
	defer g.refresh.Unlock()
	g.refreshUntil = 0
	if err := g.Check(); err == nil {
		return nil
	}
	// A partial initial accept deliberately stays missing on later calls.
	if len(g.observations) == 0 {
		r, err := g.readAttempt(ctx, end)
		accepted := g.accept(r)
		if err != nil {
			return err
		}
		if accepted != nil {
			return accepted
		}
	}
	if err := g.Check(); err == nil || err.Error() != "clock-lease-expired" {
		return err
	}
	g.mu.Lock()
	until := end
	for _, o := range g.observations {
		budget, err := clocks.LeaseRefreshBudget(o)
		if err != nil {
			g.mu.Unlock()
			return err
		}
		until = min(until, g.clock.Elapsed()+budget)
	}
	g.mu.Unlock()
	g.refreshUntil = until
	for g.clock.Elapsed() < until && ctx.Err() == nil {
		if err := g.pause(ctx, min(100*time.Millisecond, until-g.clock.Elapsed())); err != nil {
			return err
		}
		if g.clock.Elapsed() >= until {
			break
		}
		if until-g.clock.Elapsed() < leaseReadAttempt {
			return fault("clock-lease-expired")
		}
		wait, done := context.WithTimeout(ctx, until-g.clock.Elapsed())
		r, err := g.readAttempt(wait, until)
		done()
		accepted := g.accept(r)
		if err != nil {
			return err
		} // Unknown transport is never retried.
		if accepted != nil {
			if accepted.Error() == "clock-lease-missing" {
				continue
			}
			return accepted
		}
		if g.clock.Elapsed() >= until {
			break
		}
		if err = g.Check(); err == nil || err.Error() != "clock-lease-expired" {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fault("clock-lease-expired")
}

func leaseRestoration(op string, rollback, recovery bool) bool {
	switch op {
	case "worker-start", "control-start", "signal-start", "unfreeze", "relay-replenish":
		return true
	case "replace":
		return rollback || recovery
	case "worker-stop":
		return rollback || recovery
	case "freeze", "drain", "relay-stage", "relay-stop", "control-stop", "signal-stop":
		return rollback
	}
	return false
}

func isLeaseRefusal(err error) bool {
	if err == nil {
		return false
	}
	switch err.Error() {
	case "clock-lease-missing", "clock-lease-expired", "clock-lease-bound", "clock-config-differ":
		return true
	}
	return false
}

func (g *LeaseGuard) readAttempt(ctx context.Context, end time.Duration) (LeaseRead, error) {
	if end-g.clock.Elapsed() < leaseReadAttempt {
		return LeaseRead{}, fault("clock-lease-expired")
	}
	bounded, cancel := context.WithTimeout(ctx, leaseReadAttempt)
	defer cancel()
	g.mu.Lock()
	g.reading = true
	g.mu.Unlock()
	return g.read(bounded, end)
}

// coverRestoration never admits an operation: restoration has already completed.
// An expired cache needs one fresh endpoint lease plus a continuous retrospective
// tracking chain. Final coverage must still close and bound the actual event.
func (g *LeaseGuard) coverRestoration(ctx context.Context, end time.Duration, start, finish time.Time) error {
	g.refresh.Lock()
	defer g.refresh.Unlock()
	err := g.Check()
	if err == nil || err.Error() != "clock-lease-expired" {
		return err
	}
	g.mu.Lock()
	if !g.historyComplete {
		g.mu.Unlock()
		return fault("clock-lease-missing")
	}
	before := clocks.NewBoundCoverageAttempt(clocks.Evidence{Clocks: g.samples})
	g.mu.Unlock()
	// Optional timing evidence cannot consume the existing restoration reserve.
	end -= RestoreReserve
	if deadline, ok := ctx.Deadline(); ok {
		end = min(end, g.clock.Elapsed()+time.Until(deadline)-RestoreReserve)
	}
	started := g.clock.Elapsed()
	read, err := g.readAttempt(ctx, end)
	accepted := g.accept(read)
	if g.restorationReadObserved != nil {
		g.restorationReadObserved(read, started, g.clock.Elapsed())
	}
	if err != nil {
		return err
	}
	if accepted != nil {
		return accepted
	}
	return g.restorationProof(before, read, start, finish)
}

func (g *LeaseGuard) restorationProof(before clocks.CoverageAttempt, read LeaseRead, start, finish time.Time) error {
	if err := g.Check(); err != nil {
		return err
	}
	if finish.Before(start) {
		return fault("clock-lease-missing")
	}
	after := clocks.NewBoundCoverageAttempt(read.Evidence)
	timelines := map[string]observe.CoverageTimeline{}
	left, right := int64(0), int64(math.MaxInt64)
	for _, slot := range []string{g.frontend, g.control} {
		old, next := before.Clocks[slot], after.Clocks[slot]
		sample := read.Evidence.Clocks[slot]
		// Both endpoints were fully accepted; empty tracking reads are still legal.
		if len(old.Rows) == 0 {
			return fault("clock-lease-missing")
		}
		if len(next.Rows) == 0 {
			return fault("clock-lease-missing")
		}
		oldLast, newLast := old.Rows[len(old.Rows)-1].UnixSeconds, next.Rows[len(next.Rows)-1].UnixSeconds
		measured, _ := strconv.ParseInt(sample.Lease.MeasuredUnixNS, 10, 64)
		if oldLast > start.Unix() || measured-g.bounds.FrontendControlBoundNS < finish.UnixNano() || newLast < oldLast {
			return fault("clock-lease-missing")
		}
		left = max(left, oldLast)
		right = min(right, newLast)
		old.Rows = append(old.Rows, next.Rows...)
		old.Breaks = append(old.Breaks, next.Breaks...)
		old.ReadComplete = next.ReadComplete
		timelines[slot] = old
	}
	// A sub-three-second prefix has no possible >60 s continuity gap. The final
	// evaluator still requires the event's padded closing rows; no tail is waived.
	if right-left < 3 {
		return nil
	}
	windows := []observe.CoverageWindow{{Kind: "move", StartUnixNS: clocks.TimestampNS((left+1)*1e9 + g.bounds.FrontendControlBoundNS), EndUnixNS: clocks.TimestampNS((right-1)*1e9 - g.bounds.FrontendControlBoundNS)}}
	results := observe.EvaluateBoundClockCoverage(timelines, g.frontend, g.control, g.bounds.FrontendControlBoundNS, g.bounds.FrontendWorkerBoundNS, g.bounds.FrontendWorkerBoundNS-g.workerBound, windows, nil)
	for _, cause := range results[0].Causes {
		switch cause {
		case "clock-coverage-bound":
			return fault("clock-lease-bound")
		case "clock-coverage-break", "clock-coverage-leap":
			return fault("clock-config-differ")
		default:
			return fault("clock-lease-missing")
		}
	}
	return nil
}
