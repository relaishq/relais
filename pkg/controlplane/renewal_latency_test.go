package controlplane

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Each command commits before delaying its reply. The keeper must still bound
// authority by the request start, including replies received out of order.
type delayedRenewalStore struct {
	*sessionstore.Memory
	mu     sync.Mutex
	delays []time.Duration
	base   time.Duration
	starts []time.Time
	expiry atomic.Int64
	late   bool
}

func (s *delayedRenewalStore) RenewControl(ctx context.Context, lease sessionstore.ControlLease, ttl time.Duration) (sessionstore.ControlLease, error) {
	s.mu.Lock()
	delay := s.base
	if len(s.delays) > 0 {
		delay, s.delays = s.delays[0], s.delays[1:]
	}
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	if s.late && !restartWait(ctx, delay) {
		return sessionstore.ControlLease{}, ctx.Err()
	}
	next, err := s.Memory.RenewControl(ctx, lease, ttl)
	if err == nil {
		for old := s.expiry.Load(); old < next.ExpiresAt.UnixNano(); old = s.expiry.Load() {
			if s.expiry.CompareAndSwap(old, next.ExpiresAt.UnixNano()) {
				break
			}
		}
	}
	if !s.late && !restartWait(ctx, delay) {
		return sessionstore.ControlLease{}, ctx.Err()
	}
	return next, err
}
func TestControlDelayedRenewalsKeepReadiness(t *testing.T) {
	if testing.Short() {
		t.Skip("samples multiple slow renewal cycles")
	}
	for _, tc := range []struct {
		name   string
		base   time.Duration
		delays []time.Duration
	}{
		{name: "single-600ms", base: 5 * time.Millisecond, delays: []time.Duration{600 * time.Millisecond}},
		{name: "steady-450ms", base: 450 * time.Millisecond},
		{name: "steady-520ms", base: 520 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &delayedRenewalStore{Memory: sessionstore.NewMemory()}
			p, _, _ := managedLeasePlane(t, store)
			epoch := p.restart.guard.Load().lease.Epoch
			store.mu.Lock()
			store.base, store.delays = tc.base, tc.delays
			store.mu.Unlock()
			until := time.Now().Add(2 * time.Second)
			var unavailable time.Duration
			previous := time.Now()
			for time.Now().Before(until) {
				time.Sleep(5 * time.Millisecond)
				now := time.Now()
				if !p.mutationReady() {
					unavailable += now.Sub(previous)
				}
				previous = now
			}
			t.Logf("latency=%s unavailable=%s", tc.name, unavailable)
			require.Zero(t, unavailable, "healthy delayed replies must not flap readiness")
			require.Equal(t, epoch, p.restart.guard.Load().lease.Epoch)
			store.mu.Lock()
			starts := append([]time.Time(nil), store.starts...)
			store.mu.Unlock()
			require.GreaterOrEqual(t, len(starts), 5)
			for i := 1; i < len(starts); i++ {
				require.Less(t, starts[i].Sub(starts[i-1]), 350*time.Millisecond, "renewals must be scheduled from request start, including 450 ms replies")
			}
		})
	}
}
func TestControlDelayedRepliesNeverExtendAuthorityPastStoreLease(t *testing.T) {
	if testing.Short() {
		t.Skip("samples randomized reply timing")
	}
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "applied-before-reply", true: "applied-after-delay"}[late], func(t *testing.T) {
			store := &delayedRenewalStore{Memory: sessionstore.NewMemory(), late: late}
			p, _, _ := managedLeasePlane(t, store)
			store.mu.Lock()
			for i := 0; i < 20; i++ {
				store.delays = append(store.delays, time.Duration((i*7919)%700)*time.Millisecond)
			}
			store.mu.Unlock()
			until := time.Now().Add(3 * time.Second)
			samples := 0
			for time.Now().Before(until) {
				guard := p.restart.guard.Load()
				expiry := store.expiry.Load()
				if guard != nil && expiry != 0 && time.Now().Before(guard.deadline) {
					require.LessOrEqual(t, guard.deadline.UnixNano(), expiry, "local authority must not outlast the store lease")
					samples++
				}
				time.Sleep(time.Millisecond)
			}
			t.Logf("authority_samples=%d", samples)
			require.Greater(t, samples, 500)
		})
	}
}

func TestControlFailedRenewalKeepsReadinessUntilDeadlineNear(t *testing.T) {
	store := &interruptedControlRenewal{Memory: sessionstore.NewMemory(), failures: make(chan struct{}, 1)}
	p, _, _ := managedLeasePlane(t, store)
	store.fail.Store(true)
	select {
	case <-store.failures:
	case <-time.After(2 * time.Second):
		t.Fatal("renewal did not fail")
	}
	require.True(t, p.mutationReady(), "an early renewal failure must not revoke acknowledged authority")
	require.Eventually(t, func() bool { return !p.mutationReady() }, time.Second, 5*time.Millisecond)
	require.True(t, p.controlAuthority(), "mutation readiness must pause before authority expires")
	store.fail.Store(false)
}

// A callback can finish after cancellation. A late claim acknowledgement must
// neither grant expired local authority nor leave the sequential claim loop idle.
type lateClaimStore struct {
	*sessionstore.Memory
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	retried chan struct{}
}

func (s *lateClaimStore) ClaimControl(ctx context.Context, holder string, ttl time.Duration) (sessionstore.ControlLease, error) {
	if s.calls.Add(1) == 1 {
		next, err := s.Memory.ClaimControl(ctx, holder, ttl)
		close(s.entered)
		<-s.release
		return next, err
	}
	close(s.retried)
	return s.Memory.ClaimControl(ctx, holder, ttl)
}
func TestControlLateClaimReplyRetriesWithoutExpiredAuthority(t *testing.T) {
	store := &lateClaimStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{}), retried: make(chan struct{})}
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.maintainControl(ctx, make(chan error, 1)) }()
	defer func() { cancel(); <-done }()
	<-store.entered
	time.Sleep(ControlLeaseTTL + 20*time.Millisecond)
	require.False(t, p.controlAuthority())
	close(store.release)
	select {
	case <-store.retried:
	case <-time.After(time.Second):
		t.Fatal("late claim reply stopped further claims")
	}
	require.Eventually(t, p.controlAuthority, time.Second, time.Millisecond)
}
