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
	for _, failure := range []error{processidentity.ErrMismatch, errors.New("permission denied")} {
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
