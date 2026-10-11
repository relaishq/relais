package relaylease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/processidentity"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func process(owner string) sessionstore.RelayProcess {
	return sessionstore.RelayProcess{Owner: owner, PID: 42, Start: "start"}
}
func config(s sessionstore.RelayLeases) Config {
	return Config{Store: s, Key: "one", Process: process("b"), TTL: 90 * time.Millisecond, Renew: 20 * time.Millisecond, Poll: 5 * time.Millisecond}
}
func TestFenceFailurePreventsActivationAndBinding(t *testing.T) {
	for _, failure := range []error{processidentity.ErrExecutable, errors.New("permission denied")} {
		s := sessionstore.NewMemory()
		a, err := s.ClaimRelay(context.Background(), "one", process("a"), time.Millisecond)
		require.NoError(t, err)
		_, err = s.ActivateRelay(context.Background(), a)
		require.NoError(t, err)
		time.Sleep(3 * time.Millisecond)
		cfg := config(s)
		signals := 0
		cfg.Fence = func(context.Context, processidentity.Identity) error { signals++; return failure }
		bound := false
		g, err := Acquire(context.Background(), cfg)
		if err == nil {
			bound = true
			g.Close()
		}
		require.ErrorIs(t, err, failure)
		require.False(t, bound)
		require.Equal(t, 1, signals)
		record, err := s.GetRelay(context.Background(), "one")
		require.NoError(t, err)
		require.Equal(t, a.Holder, record.Forwarder)
	}
}

type unavailable struct {
	sessionstore.RelayLeases
	offline atomic.Bool
}

func (s *unavailable) ClaimRelay(ctx context.Context, k string, p sessionstore.RelayProcess, ttl time.Duration) (sessionstore.RelayLease, error) {
	if s.offline.Load() {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.ClaimRelay(ctx, k, p, ttl)
}
func (s *unavailable) RenewRelay(ctx context.Context, l sessionstore.RelayLease, ttl time.Duration) (sessionstore.RelayLease, error) {
	if s.offline.Load() {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.RenewRelay(ctx, l, ttl)
}
func TestUnavailableStoreNoTakeoverAndActiveKeepsForwarding(t *testing.T) {
	s := &unavailable{RelayLeases: sessionstore.NewMemory()}
	cfg := config(s)
	cfg.Process = process("a")
	g, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer g.Close()
	s.offline.Store(true)
	time.Sleep(2 * cfg.TTL)
	require.True(t, g.Allowed(), "expiry during outage must not self-fence")
	signals := 0
	standby := config(s)
	standby.Fence = func(context.Context, processidentity.Identity) error { signals++; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	next, err := Acquire(ctx, standby)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, next)
	require.Zero(t, signals)
	// Redis becomes reachable and a successor wins before A can renew. A must
	// stop as soon as its next CAS observes that decision.
	successor, err := s.RelayLeases.ClaimRelay(context.Background(), "one", process("b"), time.Second)
	require.NoError(t, err)
	stopped := make(chan struct{})
	g.OnLost(func() { close(stopped) })
	s.offline.Store(false)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("active did not self-fence")
	}
	require.False(t, g.Allowed())
	require.ErrorIs(t, g.Err(), sessionstore.ErrLeaseLost)
	require.Greater(t, successor.Epoch, g.Lease().Epoch)
}
func TestFencesPendingHolderAndLastForwarderBeforeActivate(t *testing.T) {
	s := sessionstore.NewMemory()
	ctx := context.Background()
	a, err := s.ClaimRelay(ctx, "one", process("a"), time.Millisecond)
	require.NoError(t, err)
	_, err = s.ActivateRelay(ctx, a)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	_, err = s.ClaimRelay(ctx, "one", process("pending"), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	var fenced []processidentity.Identity
	cfg := config(s)
	cfg.Fence = func(_ context.Context, id processidentity.Identity) error { fenced = append(fenced, id); return nil }
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err)
	defer g.Close()
	require.Len(t, fenced, 2)
	current, err := s.GetRelay(ctx, "one")
	require.NoError(t, err)
	require.Equal(t, cfg.Process, current.Forwarder)
}

type activationBlip struct {
	sessionstore.RelayLeases
	activations, renewals atomic.Int64
}

func (s *activationBlip) ActivateRelay(ctx context.Context, l sessionstore.RelayLease) (sessionstore.RelayLease, error) {
	if s.activations.Add(1) <= 8 {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.ActivateRelay(ctx, l)
}
func (s *activationBlip) RenewRelay(ctx context.Context, l sessionstore.RelayLease, ttl time.Duration) (sessionstore.RelayLease, error) {
	s.renewals.Add(1)
	return s.RelayLeases.RenewRelay(ctx, l, ttl)
}
func TestActivationRetriesAfterFencingWithRenewals(t *testing.T) {
	s := &activationBlip{RelayLeases: sessionstore.NewMemory()}
	cfg := config(s)
	a, err := s.ClaimRelay(context.Background(), cfg.Key, process("a"), time.Millisecond)
	require.NoError(t, err)
	_, err = s.RelayLeases.ActivateRelay(context.Background(), a)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	fences := 0
	cfg.Fence = func(context.Context, processidentity.Identity) error { fences++; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err)
	defer g.Close()
	require.Equal(t, 1, fences)
	require.EqualValues(t, 9, s.activations.Load())
	require.Positive(t, s.renewals.Load(), "activation retry must keep renewing")
}
func TestExpiredEpochNeedsTwoSeparatedPolls(t *testing.T) {
	s := sessionstore.NewMemory()
	cfg := config(s)
	_, err := s.ClaimRelay(context.Background(), cfg.Key, process("a"), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	cfg.Fence = func(context.Context, processidentity.Identity) error { return nil }
	started := time.Now()
	g, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer g.Close()
	require.GreaterOrEqual(t, time.Since(started), cfg.Renew)
}
func TestAcquireAllowsOwnPIDOne(t *testing.T) {
	cfg := config(sessionstore.NewMemory())
	cfg.Process.PID = 1
	g, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer g.Close()
}

func (s *unavailable) GetRelay(ctx context.Context, k string) (sessionstore.RelayLease, error) {
	if s.offline.Load() {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.GetRelay(ctx, k)
}

type renewalProbe struct {
	sessionstore.RelayLeases
	failed chan time.Time
}

func (s *renewalProbe) RenewRelay(context.Context, sessionstore.RelayLease, time.Duration) (sessionstore.RelayLease, error) {
	select {
	case s.failed <- time.Now():
	default:
	}
	return sessionstore.RelayLease{}, sessionstore.ErrTransient
}
func TestFailedRenewalRetriesQuickly(t *testing.T) {
	s := &renewalProbe{RelayLeases: sessionstore.NewMemory(), failed: make(chan time.Time, 2)}
	cfg := config(s)
	cfg.TTL = DefaultTTL
	cfg.Renew = DefaultRenew
	g, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer g.Close()
	var first time.Time
	select {
	case first = <-s.failed:
	case <-time.After(time.Second):
		t.Fatal("no first renewal")
	}
	select {
	case next := <-s.failed:
		require.Less(t, next.Sub(first), 120*time.Millisecond)
	case <-time.After(120 * time.Millisecond):
		t.Fatal("failed renewal waited the normal 200ms interval")
	}
}
func TestRecoveredStoreLetsHealthyActiveRenewBeforeStandbyClaim(t *testing.T) {
	s := &unavailable{RelayLeases: sessionstore.NewMemory()}
	cfg := config(s)
	cfg.TTL = 300 * time.Millisecond
	cfg.Renew = 120 * time.Millisecond
	cfg.Process = process("a")
	active, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer active.Close()
	s.offline.Store(true)
	standbyCfg := cfg
	standbyCfg.Process = process("b")
	fences := atomic.Int64{}
	standbyCfg.Fence = func(context.Context, processidentity.Identity) error { fences.Add(1); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		g, err := Acquire(ctx, standbyCfg)
		if g != nil {
			g.Close()
		}
		done <- err
	}()
	time.Sleep(400 * time.Millisecond)
	s.offline.Store(false)
	time.Sleep(2 * cfg.Renew)
	require.True(t, active.Allowed())
	require.Zero(t, fences.Load())
	current, err := s.GetRelay(context.Background(), cfg.Key)
	require.NoError(t, err)
	require.Equal(t, cfg.Process, current.Holder)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

type activationFailure struct {
	sessionstore.RelayLeases
	failure error
}

func (s activationFailure) ActivateRelay(context.Context, sessionstore.RelayLease) (sessionstore.RelayLease, error) {
	return sessionstore.RelayLease{}, s.failure
}
func TestActivationRetryStopsOnLossOrCancellation(t *testing.T) {
	for _, failure := range []error{sessionstore.ErrLeaseLost, sessionstore.ErrTransient} {
		s := activationFailure{RelayLeases: sessionstore.NewMemory(), failure: failure}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		g, err := Acquire(ctx, config(s))
		cancel()
		require.Nil(t, g)
		if errors.Is(failure, sessionstore.ErrLeaseLost) {
			require.ErrorIs(t, err, failure)
		} else {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}
	}
}
