package relaylease

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relais/internal/processidentity"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func process(owner string) sessionstore.RelayProcess {
	return sessionstore.RelayProcess{Owner: owner, PID: 42, Start: "start", Host: "test-host", Namespace: "test-namespace", Executable: "test-executable"}
}
func config(s sessionstore.RelayLeases) Config {
	return Config{Store: s, Key: "one", Process: process("b"), TTL: 90 * time.Millisecond, Renew: 20 * time.Millisecond, Poll: 5 * time.Millisecond, Gone: func(context.Context, processidentity.Identity) (bool, error) { return false, nil }}
}

func TestUnknownHostStartupAndRestart(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("expired=%t/plain=%t", expired, allow), func(t *testing.T) {
				s := sessionstore.NewMemory()
				old := process("unknown")
				old.Host = processidentity.UnknownHost
				l, err := s.ClaimRelay(context.Background(), "one", old, time.Second)
				require.NoError(t, err)
				_, err = s.ActivateRelay(context.Background(), l)
				require.NoError(t, err)
				if expired {
					_, err = s.RenewRelay(context.Background(), l, time.Millisecond)
					require.NoError(t, err)
					time.Sleep(3 * time.Millisecond)
				}
				cfg := config(s)
				cfg.AllowUnknownHostRestart = allow
				probes, signals := 0, 0
				cfg.Gone = func(context.Context, processidentity.Identity) (bool, error) {
					probes++
					return false, errors.New("unknown host must never be probed")
				}
				cfg.Fence = func(context.Context, processidentity.Identity) error {
					signals++
					return errors.New("unknown host must never be fenced")
				}
				g, err := Acquire(context.Background(), cfg)
				if g != nil {
					defer g.Close()
				}
				require.Zero(t, probes)
				require.Zero(t, signals)
				if allow && expired {
					require.NoError(t, err)
					require.True(t, g.Allowed())
					require.Greater(t, g.Lease().Epoch, l.Epoch)
				} else {
					require.ErrorIs(t, err, processidentity.ErrForeignScope)
					current, err := s.GetRelay(context.Background(), "one")
					require.NoError(t, err)
					require.Equal(t, l.Holder, current.Holder)
					require.Equal(t, l.Epoch, current.Epoch)
				}
			})
		}
	}
	for _, allow := range []bool{false, true} {
		cfg := config(sessionstore.NewMemory())
		cfg.Process.Host = processidentity.UnknownHost
		cfg.AllowUnknownHostRestart = allow
		g, err := Acquire(context.Background(), cfg)
		if allow {
			require.NoError(t, err)
			require.True(t, g.Allowed())
			g.Close()
		} else {
			require.ErrorIs(t, err, processidentity.ErrForeignScope)
		}
	}
}

type unknownClaimReplyLost struct {
	sessionstore.RelayLeases
	calls int
}

func (s *unknownClaimReplyLost) ClaimExpiredUnknownRelay(ctx context.Context, l sessionstore.RelayLease, p sessionstore.RelayProcess, ttl time.Duration) (sessionstore.RelayLease, error) {
	s.calls++
	claimed, err := s.RelayLeases.ClaimExpiredUnknownRelay(ctx, l, p, ttl)
	if err == nil && s.calls == 1 {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return claimed, err
}

func TestUnknownHostRestartSettlesUncertainClaim(t *testing.T) {
	for _, unknownCandidate := range []bool{false, true} {
		s := &unknownClaimReplyLost{RelayLeases: sessionstore.NewMemory()}
		old := process("old")
		old.Host = processidentity.UnknownHost
		l, err := s.ClaimRelay(context.Background(), "one", old, time.Millisecond)
		require.NoError(t, err)
		_, err = s.ActivateRelay(context.Background(), l)
		require.NoError(t, err)
		time.Sleep(3 * time.Millisecond)
		cfg := config(s)
		cfg.AllowUnknownHostRestart = true
		if unknownCandidate {
			cfg.Process.Host = processidentity.UnknownHost
		}
		g, err := Acquire(context.Background(), cfg)
		require.NoError(t, err)
		require.Equal(t, 2, s.calls)
		require.Equal(t, l.Epoch+1, g.Lease().Epoch)
		require.EqualValues(t, 1, g.Timing().StoreFailures)
		g.Close()
	}
}

func TestPlainRestartAfterUnknownClaimBeforeActivation(t *testing.T) {
	for _, forwarder := range []bool{false, true} {
		for _, mode := range []string{"plain", "standby", "foreign-host", "foreign-namespace", "unknown-local", "fence-denied"} {
			t.Run(fmt.Sprintf("forwarder=%t/%s", forwarder, mode), func(t *testing.T) {
				s := sessionstore.NewMemory()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				unknown := process("A")
				unknown.Host = processidentity.UnknownHost
				l, err := s.ClaimRelay(ctx, "one", unknown, time.Second)
				require.NoError(t, err)
				if forwarder {
					l, err = s.ActivateRelay(ctx, l)
					require.NoError(t, err)
				}
				_, err = s.RenewRelay(ctx, l, time.Millisecond)
				require.NoError(t, err)
				time.Sleep(3 * time.Millisecond)
				known := process("K")
				switch mode {
				case "foreign-host":
					known.Host = "foreign"
				case "foreign-namespace":
					known.Namespace = "foreign"
				}
				// K claims, but never activates or replaces unknown forwarder A.
				pending, err := s.ClaimExpiredUnknownRelay(ctx, l, known, time.Millisecond)
				require.NoError(t, err)
				require.Equal(t, unknown, pending.PreviousHolder)
				time.Sleep(3 * time.Millisecond)
				cfg := config(s)
				cfg.AllowUnknownHostRestart = mode != "standby"
				if mode == "unknown-local" {
					cfg.Process.Host = processidentity.UnknownHost
				}
				probes := 0
				cfg.Gone = func(context.Context, processidentity.Identity) (bool, error) { probes++; return false, nil }
				var fenced []processidentity.Identity
				cfg.Fence = func(_ context.Context, id processidentity.Identity) error {
					fenced = append(fenced, id)
					if mode == "fence-denied" {
						return syscall.EPERM
					}
					return nil
				}
				g, err := Acquire(ctx, cfg)
				if g != nil {
					defer g.Close()
				}
				require.Zero(t, probes, "unknown evidence must disable the dead-holder fast path")
				switch mode {
				case "plain":
					require.NoError(t, err)
					require.True(t, g.Allowed())
					require.Equal(t, pending.Epoch+1, g.Lease().Epoch)
					require.Equal(t, []processidentity.Identity{known.Identity()}, fenced)
					current, err := s.GetRelay(ctx, "one")
					require.NoError(t, err)
					require.Equal(t, cfg.Process, current.Forwarder)
				case "fence-denied":
					require.ErrorIs(t, err, syscall.EPERM)
					require.Equal(t, []processidentity.Identity{known.Identity()}, fenced)
				default:
					require.ErrorIs(t, err, processidentity.ErrForeignScope)
					require.Empty(t, fenced)
					current, err := s.GetRelay(ctx, "one")
					require.NoError(t, err)
					require.Equal(t, pending.Holder, current.Holder)
					require.Equal(t, pending.Epoch, current.Epoch)
				}
			})
		}
	}
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
func TestRecoveredExpiredEpochNeedsFullReachableTTL(t *testing.T) {
	s := sessionstore.NewMemory()
	cfg := config(&firstReadUnavailable{RelayLeases: s})
	_, err := s.ClaimRelay(context.Background(), cfg.Key, process("a"), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	cfg.Fence = func(context.Context, processidentity.Identity) error { return nil }
	started := time.Now()
	g, err := Acquire(context.Background(), cfg)
	require.NoError(t, err)
	defer g.Close()
	require.GreaterOrEqual(t, time.Since(started), cfg.TTL)
}
func TestContinuousStoreAccessWaitsForReachableStaleness(t *testing.T) {
	s := sessionstore.NewMemory()
	cfg := config(s)
	_, err := s.ClaimRelay(context.Background(), cfg.Key, process("a"), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	cfg.Fence = func(context.Context, processidentity.Identity) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err)
	defer g.Close()
	require.GreaterOrEqual(t, time.Since(started), cfg.TTL)
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

// A live-looking lease whose actual kernel holder has died must not force a
// candidate to wait for the TTL of reachable staleness.
func TestDeadHolderImmediatelyClaimed(t *testing.T) {
	id, err := processidentity.Current()
	require.NoError(t, err)
	old := sessionstore.RelayProcess{Owner: "old-instance", PID: id.PID, Start: "previous:" + id.Start, Host: id.Host, Namespace: id.Namespace, Executable: id.Executable}
	s := sessionstore.NewMemory()
	l, err := s.ClaimRelay(context.Background(), "one", old, 3*time.Second)
	require.NoError(t, err)
	_, err = s.ActivateRelay(context.Background(), l)
	require.NoError(t, err)
	cfg := config(s)
	cfg.TTL = 3 * time.Second
	cfg.Renew = time.Second
	cfg.Gone = nil
	cfg.Process.Host, cfg.Process.Namespace, cfg.Process.Executable = id.Host, id.Namespace, id.Executable
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err, "verifiably exited identity must bypass unexpired TTL")
	defer g.Close()
	require.Greater(t, g.Lease().Epoch, l.Epoch)
}

func TestDeadHolderFastPathRequiresDeadForwarder(t *testing.T) {
	s := sessionstore.NewMemory()
	ctx := context.Background()
	a, err := s.ClaimRelay(ctx, "one", process("a"), time.Second)
	require.NoError(t, err)
	a, err = s.ActivateRelay(ctx, a)
	require.NoError(t, err)
	pending := process("pending")
	pending.PID = 43
	pending.Start = "pending"
	l, err := s.TransferRelay(ctx, a, pending, time.Second)
	require.NoError(t, err)
	cfg := config(s)
	cfg.Gone = func(_ context.Context, id processidentity.Identity) (bool, error) { return id.PID == 43, nil }
	cfg.Fence = func(context.Context, processidentity.Identity) error {
		t.Fatal("must not fence live forwarder before expiry")
		return nil
	}
	limit, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	g, err := Acquire(limit, cfg)
	require.Nil(t, g)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	current, err := s.GetRelay(ctx, "one")
	require.NoError(t, err)
	require.Equal(t, l.Holder, current.Holder)
}
func TestUncertainGoneWaitsForExpiry(t *testing.T) {
	for _, uncertain := range []error{syscall.EPERM, errors.New("unconfirmed exit")} {
		s := sessionstore.NewMemory()
		ctx := context.Background()
		l, err := s.ClaimRelay(ctx, "one", process("a"), time.Second)
		require.NoError(t, err)
		cfg := config(s)
		cfg.Gone = func(context.Context, processidentity.Identity) (bool, error) { return false, uncertain }
		cfg.Fence = func(context.Context, processidentity.Identity) error {
			t.Fatal("uncertain identity must not signal before expiry")
			return nil
		}
		limit, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		g, err := Acquire(limit, cfg)
		cancel()
		require.Nil(t, g)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		current, err := s.GetRelay(ctx, "one")
		require.NoError(t, err)
		require.Equal(t, l.Holder, current.Holder)
	}
}
func TestChangedHolderDuringGoneCheckIsReevaluated(t *testing.T) {
	s := sessionstore.NewMemory()
	ctx := context.Background()
	old, err := s.ClaimRelay(ctx, "one", process("a"), time.Second)
	require.NoError(t, err)
	next := process("changed")
	next.PID = 43
	next.Start = "changed"
	cfg := config(s)
	checks := 0
	cfg.Gone = func(_ context.Context, id processidentity.Identity) (bool, error) {
		checks++
		if id.PID == 42 {
			_, err := s.TransferRelay(ctx, old, next, time.Second)
			require.NoError(t, err)
			return true, nil
		}
		return false, nil
	}
	cfg.Fence = func(context.Context, processidentity.Identity) error {
		t.Fatal("stale process proof must not authorize fencing")
		return nil
	}
	limit, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	g, err := Acquire(limit, cfg)
	require.Nil(t, g)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	current, err := s.GetRelay(ctx, "one")
	require.NoError(t, err)
	require.Equal(t, next, current.Holder)
	require.Greater(t, checks, 1, "changed record must cause a fresh identity check")
}
func (s *unavailable) ClaimDeadRelay(ctx context.Context, l sessionstore.RelayLease, p sessionstore.RelayProcess, ttl time.Duration) (sessionstore.RelayLease, error) {
	if s.offline.Load() {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.ClaimDeadRelay(ctx, l, p, ttl)
}

type firstReadUnavailable struct {
	sessionstore.RelayLeases
	failed bool
}

func (s *firstReadUnavailable) GetRelay(ctx context.Context, key string) (sessionstore.RelayLease, error) {
	if !s.failed {
		s.failed = true
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	return s.RelayLeases.GetRelay(ctx, key)
}

func TestActiveOnlyStorePartitionIsFencedAfterReachableStaleness(t *testing.T) {
	base := sessionstore.NewMemory()
	s := &unavailable{RelayLeases: base}
	activeCfg := config(s)
	activeCfg.Process = process("a")
	active, err := Acquire(context.Background(), activeCfg)
	require.NoError(t, err)
	defer active.Close()
	s.offline.Store(true)
	time.Sleep(activeCfg.TTL + 20*time.Millisecond)
	require.True(t, active.Allowed())
	cfg := config(base)
	cfg.TTL, cfg.Renew = activeCfg.TTL, activeCfg.Renew
	fences := 0
	cfg.Fence = func(_ context.Context, id processidentity.Identity) error {
		require.Equal(t, active.Lease().Holder.Identity(), id)
		current, err := base.GetRelay(context.Background(), cfg.Key)
		require.NoError(t, err)
		require.Equal(t, active.Lease().Holder, current.Forwarder, "activation must wait for predecessor fencing")
		fences++
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), cfg.TTL)
	defer g.Close()
	require.Equal(t, 1, fences)
	require.Zero(t, g.Timing().StoreFailures)
	current, err := base.GetRelay(context.Background(), cfg.Key)
	require.NoError(t, err)
	require.Greater(t, current.Epoch, active.Lease().Epoch)
	require.Equal(t, cfg.Process, current.Forwarder)
	s.offline.Store(false)
	require.Eventually(t, func() bool { return !active.Allowed() }, time.Second, time.Millisecond)
	require.ErrorIs(t, active.Err(), sessionstore.ErrLeaseLost)
}

type recreatingLease struct {
	sessionstore.RelayLeases
	old   sessionstore.RelayLease
	reads int
}

func (s *recreatingLease) GetRelay(ctx context.Context, key string) (sessionstore.RelayLease, error) {
	s.reads++
	if s.reads == 1 {
		return sessionstore.RelayLease{}, sessionstore.ErrTransient
	}
	if s.reads == 3 {
		if _, err := s.RenewRelay(ctx, s.old, time.Second); err != nil {
			return sessionstore.RelayLease{}, err
		}
	}
	return s.RelayLeases.GetRelay(ctx, key)
}
func TestRecoveredMissingRecordLetsActiveRecreate(t *testing.T) {
	s := &recreatingLease{RelayLeases: sessionstore.NewMemory(), old: sessionstore.RelayLease{Key: "one", Holder: process("a"), Epoch: 7}}
	cfg := config(s)
	cfg.TTL, cfg.Renew = time.Second, 200*time.Millisecond
	cfg.Fence = func(context.Context, processidentity.Identity) error {
		t.Fatal("must give the healthy active time to recreate its lease")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	g, err := Acquire(ctx, cfg)
	if g != nil {
		g.Close()
	}
	require.Nil(t, g)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	current, err := s.GetRelay(context.Background(), cfg.Key)
	require.NoError(t, err)
	require.Equal(t, s.old.Holder, current.Holder)
}
func TestContinuousStoreAccessClaimsMissingWithoutGrace(t *testing.T) {
	cfg := config(sessionstore.NewMemory())
	cfg.TTL, cfg.Renew = time.Second, 200*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	g, err := Acquire(ctx, cfg)
	require.NoError(t, err)
	defer g.Close()
}
func TestForeignHolderOrForwarderRefusesClaim(t *testing.T) {
	for _, field := range []string{"host", "namespace", "forwarder"} {
		t.Run(field, func(t *testing.T) {
			s := sessionstore.NewMemory()
			p := process("foreign")
			if field == "namespace" {
				p.Namespace = "another-namespace"
			} else {
				p.Host = "another-host"
			}
			ttl := time.Millisecond
			if field == "forwarder" {
				ttl = time.Second
			}
			l, err := s.ClaimRelay(context.Background(), "one", p, ttl)
			require.NoError(t, err)
			if field == "forwarder" {
				l, err = s.ActivateRelay(context.Background(), l)
				require.NoError(t, err)
				l, err = s.TransferRelay(context.Background(), l, process("pending"), time.Millisecond)
				require.NoError(t, err)
			}
			time.Sleep(3 * time.Millisecond)
			cfg := config(s)
			cfg.Gone = func(context.Context, processidentity.Identity) (bool, error) {
				t.Fatal("foreign scope must be rejected before reading local PIDs")
				return true, nil
			}
			cfg.Fence = func(context.Context, processidentity.Identity) error {
				t.Fatal("foreign process must never be signalled")
				return nil
			}
			before := ConfigurationErrorCount()
			g, err := Acquire(context.Background(), cfg)
			require.Nil(t, g)
			require.ErrorIs(t, err, processidentity.ErrForeignScope)
			require.Equal(t, before+1, ConfigurationErrorCount())
			current, err := s.GetRelay(context.Background(), "one")
			require.NoError(t, err)
			require.Equal(t, l.Epoch, current.Epoch)
			require.Equal(t, l.Holder, current.Holder)
		})
	}
}

func TestDefaultRelayLeaseKeepsFrozenHolderBudget(t *testing.T) {
	require.Equal(t, 600*time.Millisecond, DefaultTTL)
	cfg := config(sessionstore.NewMemory())
	cfg.TTL = 0
	cfg.Renew = 0
	g, e := Acquire(context.Background(), cfg)
	require.NoError(t, e)
	defer g.Close()
	require.InDelta(t, .6, time.Until(g.Lease().ExpiresAt).Seconds(), .05)
}
