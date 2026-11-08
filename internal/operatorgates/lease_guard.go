package operatorgates

import (
	"context"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
)

type LeaseRead struct {
	Evidence clocks.Evidence
	Sent     map[string]time.Duration
}

// LeaseGuard is prepared for atomic activation in 5b6. No verdict path creates
// one yet. Its reader must timestamp request-send after the exchange lock.
type LeaseGuard struct {
	mu, refresh               sync.Mutex
	clock                     Clock
	read                      func(context.Context, time.Duration) (LeaseRead, error)
	pause                     func(context.Context, time.Duration) error
	leases                    map[string]clocks.Lease
	observations              map[string]clocks.LeaseObservation
	bounds                    clocks.Bounds
	frontend, control, growth string
	workerBound               int64
	frozenEnd                 int64
	invalid                   bool
}

func NewLeaseGuard(e clocks.CoverageEvidence, session *Session, read func(context.Context, time.Duration) (LeaseRead, error), pause func(context.Context, time.Duration) error) (*LeaseGuard, error) {
	if e.Binding == nil || e.ValidateBinding(e.ClockReceiptSHA256) != nil || session == nil || session.clock == nil || read == nil || pause == nil {
		return nil, fault("clock-config-differ")
	}
	r := e.Binding.Receipt
	return &LeaseGuard{clock: session.clock, read: read, pause: pause, leases: map[string]clocks.Lease{}, observations: map[string]clocks.LeaseObservation{}, bounds: r.Bounds, frontend: r.FrontendSlot, control: r.ControlSlot, growth: r.Policy.MaxDispersionGrowthPPM, workerBound: r.Bounds.FrontendWorkerBoundNS - r.Policy.SignalReserveNS, frozenEnd: r.EndUnixSeconds}, nil
}

func (g *LeaseGuard) accept(r LeaseRead) error {
	g.mu.Lock()
	defer g.mu.Unlock()
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
	}
	return nil
}

// Check is pure with respect to host I/O, so it can run after arm and while
// holding the send lock. It must never try to refresh from that lock.
func (g *LeaseGuard) Check() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.invalid {
		return fault("clock-config-differ")
	}
	values := map[string]int64{}
	declared, _ := new(big.Rat).SetString(g.growth)
	for _, slot := range inventory.Slots {
		v, err := g.leases[slot].At(g.clock.Elapsed())
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
	if err := g.Check(); err == nil {
		return nil
	}
	// A partial initial accept deliberately stays missing on later calls.
	if len(g.observations) == 0 {
		r, err := g.read(ctx, end)
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
	for g.clock.Elapsed() < until && ctx.Err() == nil {
		if err := g.pause(ctx, min(100*time.Millisecond, until-g.clock.Elapsed())); err != nil {
			return err
		}
		if g.clock.Elapsed() >= until {
			break
		}
		wait, done := context.WithTimeout(ctx, until-g.clock.Elapsed())
		r, err := g.read(wait, until)
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
	case "freeze", "drain", "worker-stop", "relay-stage", "relay-stop", "control-stop", "signal-stop":
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
