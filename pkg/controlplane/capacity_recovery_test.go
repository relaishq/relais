package controlplane

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestExportedMoveRecoveryUsesFullAlternate(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "rollback"
		if retained {
			name = "retained-uncertain"
		}
		t.Run(name, func(t *testing.T) {
			p, _, _, _ := setup(t)
			p.config.WorkerCallCap = 1
			source, id, _, _, c := prepareFullPoolRecovery(t, p, retained)
			require.Eventually(t, func() bool {
				pending := pendingRecovery(p, source, id)
				if pending == nil {
					return c.runs(id)
				}
				p.recovery.recover(context.Background(), source, *pending, time.Now())
				return c.runs(id)
			}, time.Second, 5*time.Millisecond)
			require.Empty(t, recoveryWork(p, source))
			status, err := p.Status(context.Background())
			require.NoError(t, err)
			require.Zero(t, status.LostCount)
			require.Equal(t, 1, status.Pool.OverCapCalls)
			require.NoError(t, p.End(context.Background(), id))
		})
	}
}

func TestMoveBudgetRecoveryReconsidersRefusingWorkers(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "rollback"
		if retained {
			name = "retained-uncertain"
		}
		t.Run(name, func(t *testing.T) {
			p, _, _, _ := setup(t)
			p.config.WorkerCallCap = 1
			source, id, a, b, c := prepareFullPoolRecovery(t, p, retained)
			c.refuse.Store(true)
			p.recovery.recover(context.Background(), source, *pendingRecovery(p, source, id), time.Now())
			pending := pendingRecovery(p, source, id)
			require.NotNil(t, pending)
			require.Greater(t, c.attempts.Load(), int64(0), "exported recovery may try an over-cap worker")
			ready := a
			if retained {
				ready = b
			}
			ready.refuse.Store(false)
			require.Eventually(t, func() bool {
				pending := pendingRecovery(p, source, id)
				if pending == nil {
					return ready.runs(id)
				}
				p.recovery.recover(context.Background(), source, *pending, time.Now())
				return ready.runs(id)
			}, time.Second, 5*time.Millisecond)
			status, err := p.Status(context.Background())
			require.NoError(t, err)
			require.Zero(t, status.LostCount)
			require.Empty(t, recoveryWork(p, source))
		})
	}
}

func prepareFullPoolRecovery(t *testing.T, p *Plane, retained bool) (*registration, string, *budgetRetryWorker, *budgetRetryWorker, *budgetRetryWorker) {
	t.Helper()
	a := p.workers["a"].worker.(*fakeWorker)
	b := p.workers["b"].worker.(*fakeWorker)
	p.relay = &trackedHoldRelay{fakeRelay: &fakeRelay{}}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	base := p.store
	c := &fakeWorker{store: base, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}
	require.NoError(t, p.Register("c", c.addr, c))
	_, _, err = p.Create(ctx, "offer", "c")
	require.NoError(t, err)
	aw, bw, cw := &budgetRetryWorker{fakeWorker: a}, &budgetRetryWorker{fakeWorker: b}, &budgetRetryWorker{fakeWorker: c}
	aw.refuse.Store(true)
	bw.refuse.Store(true)
	p.workers["a"].worker = aw
	p.workers["b"].worker = bw
	p.workers["c"].worker = cw
	if retained {
		p.store = &retainedMoveStore{Store: base}
	}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	if !retained {
		require.ErrorIs(t, err, mediaworker.ErrMoveBudget)
	}
	source := p.workers["a"]
	require.NotNil(t, pendingRecovery(p, source, id))
	p.store = base
	return source, id, aw, bw, cw
}

// Selection can also surface typed capacity errors from a store adapter. Keep
// the same bounded exclusion reset regardless of where the sentinel originated.
type capacityListingStore struct {
	sessionstore.Store
	blocked  bool
	listings int
}

func (s *capacityListingStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	s.listings++
	if s.blocked {
		return nil, ErrAtCapacity
	}
	return s.Store.ListByWorker(ctx, addr)
}
func TestMoveBudgetRecoveryBacksOffOnCapacityRefusal(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	source, id, a, _, _ := prepareFullPoolRecovery(t, p, false)
	store := &capacityListingStore{Store: p.store, blocked: true}
	p.store = store
	p.recovery.recover(context.Background(), source, *pendingRecovery(p, source, id), time.Now())
	pending := pendingRecovery(p, source, id)
	require.NotNil(t, pending)
	before := store.listings
	p.recovery.recover(context.Background(), source, *pending, time.Now())
	require.Equal(t, before, store.listings, "backoff must prevent another capacity listing on the same tick")
	store.blocked = false
	a.refuse.Store(false)
	require.Eventually(t, func() bool {
		pending := pendingRecovery(p, source, id)
		if pending == nil {
			return a.runs(id)
		}
		p.recovery.recover(context.Background(), source, *pending, time.Now())
		return a.runs(id)
	}, time.Second, 5*time.Millisecond)
}
