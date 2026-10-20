package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type slowControlLeaseStore struct {
	*sessionstore.Memory
	latency  time.Duration
	renewals atomic.Int64
}

func (s *slowControlLeaseStore) ClaimControl(ctx context.Context, holder string, ttl time.Duration) (sessionstore.ControlLease, error) {
	if !restartWait(ctx, s.latency) {
		return sessionstore.ControlLease{}, ctx.Err()
	}
	return s.Memory.ClaimControl(ctx, holder, ttl)
}
func (s *slowControlLeaseStore) RenewControl(ctx context.Context, lease sessionstore.ControlLease, ttl time.Duration) (sessionstore.ControlLease, error) {
	if !restartWait(ctx, s.latency) {
		return sessionstore.ControlLease{}, ctx.Err()
	}
	next, err := s.Memory.RenewControl(ctx, lease, ttl)
	if err == nil {
		s.renewals.Add(1)
	}
	return next, err
}

func TestControlSlowHealthyStoreKeepsReadiness(t *testing.T) {
	store := &slowControlLeaseStore{Memory: sessionstore.NewMemory(), latency: 300 * time.Millisecond}
	p, done, _ := managedLeasePlane(t, store)
	epoch := p.restart.guard.Load().lease.Epoch
	require.Never(t, func() bool { return !p.mutationReady() }, 3*time.Second, 10*time.Millisecond, "300 ms store latency must not flap mutation readiness")
	require.GreaterOrEqual(t, store.renewals.Load(), int64(5), "must observe repeated successful renewals")
	require.Equal(t, epoch, p.restart.guard.Load().lease.Epoch)
	select {
	case err := <-done:
		done <- err
		t.Fatalf("slow healthy store stopped control: %v", err)
	default:
	}
}

// Crash immediately after a claim so the successor must wait nearly the whole
// TTL, rather than relying on a favorable point in the renewal cycle.
func TestControlFreshPredecessorLeaseLeavesRecoveryHeadroom(t *testing.T) {
	store := sessionstore.NewMemory()
	_, err := store.ClaimControl(context.Background(), "crashed-predecessor", ControlLeaseTTL)
	require.NoError(t, err)
	started := time.Now()
	_, _, _ = managedLeasePlane(t, store)
	elapsed := time.Since(started)
	require.Less(t, elapsed, 2234400*time.Microsecond-500*time.Millisecond, "fresh predecessor lease must leave at least 500 ms inside the default snapshot-age budget")
}
