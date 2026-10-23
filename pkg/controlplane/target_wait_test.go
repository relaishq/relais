package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type inventoryWorker struct {
	*fakeWorker
	inventory []mediaworker.InstalledSession
	truncated bool
}

func (w *inventoryWorker) Status(context.Context) (mediaworker.WorkerStatus, error) {
	return mediaworker.WorkerStatus{Address: w.addr, InventoryKnown: true, Truncated: w.truncated,
		Inventory: append([]mediaworker.InstalledSession(nil), w.inventory...)}, nil
}

type stalledPool struct {
	p     *Plane
	a     *inventoryWorker
	b     *takeoverWorker
	id    string
	lease sessionstore.Lease
	now   time.Time
}

func newStalledPool(t *testing.T) *stalledPool {
	t.Helper()
	p, a, b, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(context.Background(), lease, takeoverSnapshot(t, id, 0)))
	s := &stalledPool{p: p, a: &inventoryWorker{fakeWorker: a, inventory: []mediaworker.InstalledSession{{ID: id, Owner: a.addr, Epoch: lease.Epoch}}}, b: &takeoverWorker{fakeWorker: b}, id: id, lease: lease, now: time.Now()}
	p.workers["a"].worker, p.workers["b"].worker = s.a, s.b
	for _, w := range p.workers {
		w.observation = newWorkerObservation(s.now.Add(-450*time.Millisecond), false)
		require.True(t, w.observation.detect(s.now, p.config.DeadAfter).newDeath)
	}
	p.recovery.targetNow = func() time.Time { return s.now }
	return s
}

func (s *stalledPool) sourceReturns(t *testing.T) {
	t.Helper()
	_, _, err := s.p.workers["a"].observation.heartbeat(workerHeartbeat{}, s.now)
	require.ErrorContains(t, err, "takeover in progress")
}

func (s *stalledPool) targetReturns(t *testing.T) {
	t.Helper()
	o := &s.p.workers["b"].observation
	o.recoveryFinished(true)
	_, _, err := o.heartbeat(workerHeartbeat{}, s.now)
	require.ErrorIs(t, err, mediaworker.ErrRejoinRequired)
	_, _, err = o.heartbeat(workerHeartbeat{}, s.now)
	require.NoError(t, err)
	s.p.observationChangedLocked()
}

func TestTargetWaitHostStall(t *testing.T) {
	for _, returning := range []string{"source", "source-and-target", "target"} {
		t.Run(returning, func(t *testing.T) {
			s := newStalledPool(t)
			waits := 0
			s.p.recovery.targetWait = func(_ context.Context, _ <-chan struct{}, _ time.Duration) error {
				waits++
				require.Equal(t, 1, waits)
				require.Zero(t, s.p.lostCount)
				require.True(t, s.a.runs(s.id))
				current, err := s.p.store.Get(context.Background(), s.id)
				require.NoError(t, err)
				require.Equal(t, s.lease.Epoch, current.Epoch)
				s.now = s.now.Add(200 * time.Millisecond) // exceeds the old three-selection window
				s.p.mu.Lock()
				defer s.p.mu.Unlock()
				if returning != "target" {
					s.sourceReturns(t)
				}
				if returning != "source" {
					s.targetReturns(t)
				}
				return nil
			}
			require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
			require.Zero(t, s.p.lostCount, "the host stall must not lose the call")
			require.Equal(t, 1, waits)
			current, err := s.p.store.Get(context.Background(), s.id)
			require.NoError(t, err)
			status, err := s.p.Status(context.Background())
			require.NoError(t, err)
			require.Zero(t, status.LostCount)
			if returning == "target" {
				require.Equal(t, s.b.addr, current.Worker)
				require.Greater(t, current.Epoch, s.lease.Epoch)
				require.True(t, s.b.runs(s.id))
				require.Len(t, status.Takeovers, 1)
			} else {
				require.Equal(t, s.a.addr, current.Worker)
				require.Equal(t, s.lease.Epoch, current.Epoch)
				require.True(t, s.a.runs(s.id))
				require.Empty(t, status.Takeovers)
				require.True(t, s.p.workers["a"].observation.knowledge().eligible)
			}
		})
	}
}

func TestTargetWaitDeadlineAndCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "cancel-and-retry"}[cancelFirst], func(t *testing.T) {
			s := newStalledPool(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deadline time.Time
			waits := 0
			s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
				waits++
				pending := s.p.recovery.pending[s.p.workers["a"]][s.id]
				pending.agentFailure = errors.New("target agent incompatible")
				if deadline.IsZero() {
					deadline = pending.targetDeadline
				}
				require.Equal(t, deadline, pending.targetDeadline)
				if cancelFirst && waits == 1 {
					s.now = s.now.Add(200 * time.Millisecond)
					cancel()
					return context.Canceled
				}
				s.now = deadline
				return nil
			}
			s.p.recovery.recover(ctx, s.p.workers["a"], s.lease, s.now)
			if cancelFirst {
				require.Zero(t, s.p.lostCount)
				require.True(t, s.a.runs(s.id))
				s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now)
			}
			status, err := s.p.Status(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, status.LostCount)
			require.Len(t, status.Takeovers, 1)
			require.Contains(t, status.Takeovers[0].Error, ErrTargetWaitExpired.Error())
			require.Contains(t, status.Takeovers[0].Error, "target agent incompatible")
			require.Equal(t, "recovery_failed", status.Takeovers[0].LossCause)
			require.Empty(t, status.Calls)
		})
	}
}

func TestTargetWaitPrefersSourceBeforeFirstSelection(t *testing.T) {
	s := newStalledPool(t)
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	s.targetReturns(t)
	s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
		t.Fatal("live source must not wait or move")
		return nil
	}
	require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now.Add(-100*time.Millisecond)))
	lease, err := s.p.store.Get(context.Background(), s.id)
	require.NoError(t, err)
	require.Equal(t, s.lease.Epoch, lease.Epoch)
	require.Equal(t, s.a.addr, lease.Worker)
	require.Empty(t, s.p.recentTakeovers())
}

func TestTargetWaitEligibleCrashDoesNotWait(t *testing.T) {
	s := newStalledPool(t)
	s.targetReturns(t)
	s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
		t.Fatal("eligible target must transfer immediately")
		return nil
	}
	require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
	require.True(t, s.b.runs(s.id))
	require.Zero(t, s.p.lostCount)
}

type interruptedTransferStore struct {
	sessionstore.Store
	uncertain, interrupted, failGet bool
}

func (s *interruptedTransferStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	if s.interrupted {
		return s.Store.Transfer(ctx, from, to, ttl)
	}
	s.interrupted = true
	if !s.uncertain {
		return sessionstore.Lease{}, errors.New("transfer request interrupted")
	}
	lease, err := s.Store.Transfer(ctx, from, to, ttl)
	if err != nil {
		return lease, err
	}
	s.failGet = true
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "transfer", Err: errors.New("reply lost"), Candidate: &lease}
}
func (s *interruptedTransferStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.failGet {
		s.failGet = false
		return sessionstore.Lease{}, errors.New("settlement read interrupted")
	}
	return s.Store.Get(ctx, id)
}

func TestTargetWaitIssuedTransferNeverAbandons(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "issued", true: "uncertain-commit"}[uncertain], func(t *testing.T) {
			s := newStalledPool(t)
			s.targetReturns(t)
			s.p.store = &interruptedTransferStore{Store: s.p.store, uncertain: uncertain}
			require.Nil(t, s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now))
			s.now = s.now.Add(100 * time.Millisecond)
			s.sourceReturns(t)
			require.NotNil(t, s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now))
			lease, err := s.p.store.Get(context.Background(), s.id)
			require.NoError(t, err)
			require.Equal(t, s.b.addr, lease.Worker)
			require.Greater(t, lease.Epoch, s.lease.Epoch)
			require.False(t, s.p.workers["a"].observation.preserving)
			require.True(t, s.b.runs(s.id))
		})
	}
}

func TestTargetWaitMixedCallsWaitForSelfFence(t *testing.T) {
	s := newStalledPool(t)
	second, _, err := s.p.Create(context.Background(), "offer", "a")
	// Create is deliberately unavailable during recovery. Seed a second exact
	// tenure as an already-admitted call, using the same fake protocol boundary.
	require.Error(t, err)
	require.Empty(t, second)
	second = "survivor"
	lease, err := s.p.store.Claim(context.Background(), second, s.a.addr, time.Minute)
	require.NoError(t, err)
	s.p.calls[second] = &call{id: second, lease: lease}
	s.a.running[second] = true
	s.a.inventory = append(s.a.inventory, mediaworker.InstalledSession{ID: second, Owner: s.a.addr, Epoch: lease.Epoch})
	s.targetReturns(t)
	require.NotNil(t, s.p.recovery.recover(context.Background(), s.p.workers["a"], s.lease, s.now))
	s.now = s.now.Add(100 * time.Millisecond)
	s.sourceReturns(t)
	require.False(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
	require.True(t, s.p.workers["a"].observation.preserving)
	require.False(t, s.p.workers["a"].observation.knowledge().eligible)
	_, _, err = s.p.workers["a"].observation.heartbeat(workerHeartbeat{}, s.now)
	require.ErrorContains(t, err, "takeover in progress")
	require.True(t, s.a.runs(second))
	// The real worker does this on checkpoint ErrLeaseLost; retain only the
	// still-authoritative inventory, without ever delivering a rejoin challenge.
	delete(s.a.running, s.id)
	s.a.inventory = s.a.inventory[1:]
	require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
	require.True(t, s.a.runs(second))
	require.True(t, s.p.workers["a"].observation.knowledge().eligible)
	current, err := s.p.store.Get(context.Background(), second)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, current.Epoch)
	require.Zero(t, s.p.lostCount)
}

func TestTargetWaitDeadlineBounds(t *testing.T) {
	for _, bound := range []string{"detection", "heartbeat", "parent", "missing-heartbeat"} {
		t.Run(bound, func(t *testing.T) {
			s := newStalledPool(t)
			ctx := context.Background()
			expected := s.now.Add(takeoverBudget)
			switch bound {
			case "heartbeat":
				s.p.workers["a"].observation.deathHeartbeat = s.now.Add(-900 * time.Millisecond)
				expected = s.now.Add(1050 * time.Millisecond)
			case "parent":
				var stop context.CancelFunc
				expected = s.now.Add(700 * time.Millisecond)
				ctx, stop = context.WithDeadline(ctx, expected)
				defer stop()
			case "missing-heartbeat":
				s.p.workers["a"].observation.lastHeartbeat = time.Time{}
				s.p.workers["a"].observation.deathHeartbeat = time.Time{}
			}
			called := false
			s.p.recovery.targetWait = func(context.Context, <-chan struct{}, time.Duration) error {
				called = true
				require.Equal(t, expected, s.p.recovery.pending[s.p.workers["a"]][s.id].targetDeadline)
				return context.Canceled
			}
			s.p.recovery.recover(ctx, s.p.workers["a"], s.lease, s.now)
			require.True(t, called)
			require.Zero(t, s.p.lostCount)
		})
	}
}

func TestTargetWaitIncompleteInventoryCannotAbandon(t *testing.T) {
	for _, invalid := range []string{"truncated", "wrong-epoch"} {
		t.Run(invalid, func(t *testing.T) {
			s := newStalledPool(t)
			s.now = s.now.Add(100 * time.Millisecond)
			s.sourceReturns(t)
			s.targetReturns(t)
			if invalid == "truncated" {
				s.a.truncated = true
			} else {
				s.a.inventory[0].Epoch++
			}
			require.True(t, s.p.recoverWorker(context.Background(), s.p.workers["a"], s.now))
			current, err := s.p.store.Get(context.Background(), s.id)
			require.NoError(t, err)
			require.Equal(t, s.b.addr, current.Worker)
			require.True(t, s.b.runs(s.id))
			require.False(t, s.p.workers["a"].observation.preserving)
		})
	}
}

func TestTargetWaitObservationWakesWithoutPolling(t *testing.T) {
	changed := make(chan struct{})
	close(changed)
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	require.NoError(t, waitTargetObservation(ctx, changed, time.Hour, time.Now().Add(time.Hour)))
}

type privateInventoryWorker struct {
	*fakeWorker
	handler http.Handler
}

func (w *privateInventoryWorker) PrivateHandler() http.Handler { return w.handler }

func TestRecoveryInventoryUsesExistingPrivateHandler(t *testing.T) {
	_, a, _, _ := setup(t)
	w := &privateInventoryWorker{fakeWorker: a, handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/status", r.URL.Path)
		rw.Header().Set("Content-Type", "application/json")
		_, err := rw.Write([]byte(`{"inventory_known":true,"inventory":[],"address":"127.0.0.1:1"}`))
		require.NoError(t, err)
	})}
	status, err := readRecoveryInventory(context.Background(), w)
	require.NoError(t, err)
	require.True(t, status.InventoryKnown)
	require.Equal(t, a.addr, status.Address)
}
