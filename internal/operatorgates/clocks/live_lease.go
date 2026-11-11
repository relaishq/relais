package clocks

import (
	"context"
	"math/big"
	"time"
)

// Lease keeps an immutable monotonic expiry for one reference update. A reread
// may tighten that expiry, but only a newer reference update can extend it.
type Lease struct {
	observation  LeaseObservation
	sent, expiry time.Duration
}

func LeaseRefreshBudget(o LeaseObservation) (time.Duration, error) {
	n, err := pollsNS(o, LeaseRefreshPolls)
	return time.Duration(n), err
}

func pollsNS(o LeaseObservation, polls int64) (int64, error) {
	p, err := exact(o.PollSeconds)
	if err != nil || p.Sign() <= 0 {
		return 0, &BindingError{"clock-lease-missing"}
	}
	p.Mul(p, big.NewRat(polls*1e9, 1))
	n := new(big.Int).Quo(p.Num(), p.Denom())
	if !n.IsInt64() || n.Sign() <= 0 {
		return 0, &BindingError{"clock-lease-missing"}
	}
	return n.Int64(), nil
}

// ExpiredLeaseAge reports an expired lease's aged age (reference age plus host
// elapsed time) and its validity budget. Both come from published fields.
func ExpiredLeaseAge(o LeaseObservation) (ageNS, budgetNS int64, ok bool) {
	v, err := RecordLease(o, o.HostElapsedNS)
	if err != nil || !v.Expired {
		return 0, 0, false
	}
	budgetNS, err = pollsNS(o, LeaseValidityPolls)
	return o.ReferenceAgeNS + o.HostElapsedNS, budgetNS, err == nil
}

func (l Lease) Observe(o LeaseObservation, sent, received time.Duration) (Lease, error) {
	_, err := RecordLease(o, int64(received-sent))
	if err != nil || sent < 0 {
		return Lease{}, &BindingError{"clock-lease-missing"}
	}
	// Derive update+5P from the original request, never receipt or reread time.
	p, err := exact(o.PollSeconds)
	if err != nil {
		return Lease{}, &BindingError{"clock-lease-missing"}
	}
	p.Mul(p, big.NewRat(LeaseValidityPolls*1e9, 1))
	p.Sub(p, big.NewRat(o.ReferenceAgeNS, 1))
	n := new(big.Int).Quo(p.Num(), p.Denom())
	if !n.IsInt64() || n.Int64() > int64(^uint64(0)>>1)-int64(sent) {
		return Lease{}, &BindingError{"clock-lease-missing"}
	}
	expiry := sent + time.Duration(n.Int64())
	if l.observation.Available {
		if o.ReferenceUnixNS < l.observation.ReferenceUnixNS {
			return Lease{}, &BindingError{"clock-config-differ"}
		}
		if o.ReferenceUnixNS == l.observation.ReferenceUnixNS {
			expiry = min(expiry, l.expiry)
		}
	}
	return Lease{observation: o, sent: sent, expiry: expiry}, nil
}

func (l Lease) At(now time.Duration) (RecordedLease, error) {
	v, err := RecordLease(l.observation, int64(now-l.sent))
	if err != nil {
		return v, &BindingError{"clock-lease-missing"}
	}
	v.RemainingNS = max(0, int64(l.expiry-now))
	v.Expired = v.Expired || now >= l.expiry
	return v, nil
}

// RefreshLease is for recorded admission evidence. It preserves the selector's
// host-only aging rule; live gates use Lease.At with request-send transport age.
// Once expired, only a newer reference update qualifies. The original 10P
// deadline includes every retry/read and is never reset by another stale read.
func RefreshLease(ctx context.Context, first LeaseObservation, now func() time.Time, pause func(context.Context, time.Duration) error, read func(context.Context) (LeaseObservation, error)) (LeaseObservation, error) {
	v, err := RecordLease(first, first.HostElapsedNS)
	if err != nil {
		return first, &BindingError{"clock-lease-missing"}
	}
	if !v.Expired {
		return first, nil
	}
	budget, e := LeaseRefreshBudget(first)
	if e != nil {
		return first, &BindingError{"clock-lease-missing"}
	}
	end := now().Add(budget)
	for now().Before(end) && ctx.Err() == nil {
		if e = pause(ctx, min(100*time.Millisecond, end.Sub(now()))); e != nil {
			return first, e
		}
		if !now().Before(end) {
			break
		}
		wait, done := context.WithTimeout(ctx, end.Sub(now()))
		o, readErr := read(wait)
		done()
		if !now().Before(end) {
			break
		}
		if readErr != nil {
			continue
		}
		v, err = RecordLease(o, o.HostElapsedNS)
		if err == nil && !v.Expired && o.ReferenceUnixNS > first.ReferenceUnixNS {
			return o, nil
		}
	}
	if ctx.Err() != nil {
		return first, ctx.Err()
	}
	return first, &BindingError{"clock-lease-expired"}
}
