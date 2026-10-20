package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type interruptedControlRenewal struct {
	*sessionstore.Memory
	fail     atomic.Bool
	fatal    atomic.Bool
	failures chan struct{}
}

func (s *interruptedControlRenewal) RenewControl(ctx context.Context, lease sessionstore.ControlLease, ttl time.Duration) (sessionstore.ControlLease, error) {
	if s.fatal.Load() {
		return sessionstore.ControlLease{}, sessionstore.ErrControlLimit
	}
	if s.fail.Load() {
		select {
		case s.failures <- struct{}{}:
		default:
		}
		return sessionstore.ControlLease{}, sessionstore.ErrTransient
	}
	return s.Memory.RenewControl(ctx, lease, ttl)
}
func managedLeasePlane(t *testing.T, store sessionstore.Store) (*Plane, chan error, context.CancelFunc) {
	t.Helper()
	addr := netip.MustParseAddrPort("127.0.0.1:23463")
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: addr, InventoryKnown: true})
	}))
	t.Cleanup(worker.Close)
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	p.SetRelayReady(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.RunManaged(ctx, []FixedWorker{{Name: "a", URL: worker.URL}}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("managed control did not stop")
		}
		_ = p.ReleaseControl(context.Background())
	})
	require.Eventually(t, p.mutationReady, 3*time.Second, 10*time.Millisecond)
	return p, done, cancel
}
func TestControlTransientRenewalResumesSameTenure(t *testing.T) {
	store := &interruptedControlRenewal{Memory: sessionstore.NewMemory(), failures: make(chan struct{}, 1)}
	p, done, _ := managedLeasePlane(t, store)
	epoch := p.restart.guard.Load().lease.Epoch
	store.fail.Store(true)
	select {
	case <-store.failures:
	case <-time.After(2 * time.Second):
		t.Fatal("renewal was not attempted")
	}
	require.Eventually(t, func() bool { return !p.mutationReady() }, time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		done <- err
		t.Fatalf("transient renewal stopped control: %v", err)
	default:
	}
	store.fail.Store(false)
	require.Eventually(t, p.mutationReady, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, epoch, p.restart.guard.Load().lease.Epoch)
}
func TestControlExpiredRenewalReclaimsAndRebuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for control lease expiry")
	}
	store := &interruptedControlRenewal{Memory: sessionstore.NewMemory(), failures: make(chan struct{}, 1)}
	p, done, _ := managedLeasePlane(t, store)
	epoch := p.restart.guard.Load().lease.Epoch
	store.fail.Store(true)
	require.Eventually(t, func() bool { return !p.controlAuthority() }, 3*time.Second, 20*time.Millisecond)
	require.False(t, p.mutationReady())
	select {
	case err := <-done:
		done <- err
		t.Fatalf("expired renewal stopped control: %v", err)
	default:
	}
	store.fail.Store(false)
	require.Eventually(t, p.mutationReady, 3*time.Second, 10*time.Millisecond)
	require.Greater(t, p.restart.guard.Load().lease.Epoch, epoch)
	require.Equal(t, "ready", p.RebuildStatus().Phase)
}
func TestControlUnrecoverableRenewalReturnsError(t *testing.T) {
	store := &interruptedControlRenewal{Memory: sessionstore.NewMemory(), failures: make(chan struct{}, 1)}
	p, done, _ := managedLeasePlane(t, store)
	store.fatal.Store(true)
	select {
	case err := <-done:
		require.ErrorIs(t, err, sessionstore.ErrControlLimit)
		done <- err
	case <-time.After(2 * time.Second):
		t.Fatal("unrecoverable renewal did not stop control")
	}
	require.False(t, p.mutationReady())
}

type delayedControlReply struct {
	*sessionstore.Memory
	claim   chan time.Time
	renew   chan time.Time
	release chan struct{}
}

func (s *delayedControlReply) delay(ctx context.Context, notify chan time.Time) error {
	select {
	case notify <- time.Now():
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *delayedControlReply) ClaimControl(ctx context.Context, holder string, ttl time.Duration) (sessionstore.ControlLease, error) {
	lease, err := s.Memory.ClaimControl(ctx, holder, ttl)
	if err == nil {
		err = s.delay(ctx, s.claim)
	}
	return lease, err
}
func (s *delayedControlReply) RenewControl(ctx context.Context, lease sessionstore.ControlLease, ttl time.Duration) (sessionstore.ControlLease, error) {
	next, err := s.Memory.RenewControl(ctx, lease, ttl)
	if err == nil {
		err = s.delay(ctx, s.renew)
	}
	return next, err
}
func TestControlLeaseDeadlinesUseRequestStart(t *testing.T) {
	store := &delayedControlReply{Memory: sessionstore.NewMemory(), claim: make(chan time.Time, 1), renew: make(chan time.Time, 1), release: make(chan struct{})}
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	fatal := make(chan error, 1)
	go func() { defer close(done); p.maintainControl(ctx, fatal) }()
	defer func() { cancel(); <-done }()
	for _, entered := range []chan time.Time{store.claim, store.renew} {
		var at time.Time
		select {
		case at = <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("lease request did not start")
		}
		prior := p.restart.guard.Load()
		store.release <- struct{}{}
		require.Eventually(t, func() bool { return p.restart.guard.Load() != nil && p.restart.guard.Load() != prior }, time.Second, time.Millisecond)
		require.False(t, p.restart.guard.Load().deadline.After(at.Add(ControlLeaseTTL)), "deadline used reply time")
	}
	select {
	case err := <-fatal:
		require.NoError(t, err)
	default:
	}
}

type blockedOwnershipRead struct {
	*sessionstore.Memory
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *blockedOwnershipRead) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.block.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.Memory.Get(ctx, id)
}
func TestControlShutdownRenewsUntilRecoveryStops(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a recovery job beyond one control TTL")
	}
	store := &blockedOwnershipRead{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	ctx := context.Background()
	_, err := store.Claim(ctx, "existing", netip.MustParseAddrPort("127.0.0.1:23463"), time.Minute)
	require.NoError(t, err)
	p, done, cancel := managedLeasePlane(t, store)
	store.block.Store(true)
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("ownership read not started")
	}
	cancel()
	defer close(store.release)
	require.Never(t, func() bool {
		select {
		case err := <-done:
			done <- err
			return true
		default:
			return false
		}
	}, ControlLeaseTTL+200*time.Millisecond, 50*time.Millisecond)
	_, err = store.ClaimControl(ctx, "successor", ControlLeaseTTL)
	require.ErrorIs(t, err, sessionstore.ErrLeaseHeld, "lease lapsed while recovery still ran")
	require.True(t, p.controlAuthority())
}

func TestControlShutdownPausesDetectionWhileLeaseRenews(t *testing.T) {
	if testing.Short() {
		t.Skip("observes shutdown beyond one control TTL")
	}
	store := sessionstore.NewMemory()
	p, _, _ := managedLeasePlane(t, store)
	p.BeginShutdown()
	require.False(t, p.mutationReady())
	require.Never(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.workers["a"].dead
	}, ControlLeaseTTL+200*time.Millisecond, 50*time.Millisecond)
	_, err := store.ClaimControl(context.Background(), "successor", ControlLeaseTTL)
	require.ErrorIs(t, err, sessionstore.ErrLeaseHeld)
	require.True(t, p.controlAuthority())
}
