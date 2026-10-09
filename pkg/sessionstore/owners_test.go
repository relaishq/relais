package sessionstore

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryLeases(t *testing.T) {
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:4001")
	b := netip.MustParseAddrPort("127.0.0.1:4002")
	m := NewMemory()
	_, err := m.Get(ctx, "s")
	require.ErrorIs(t, err, ErrNotFound)
	first, err := m.Claim(ctx, "s", a, time.Second)
	require.NoError(t, err)
	_, err = m.Claim(ctx, "s", b, time.Second)
	require.ErrorIs(t, err, ErrLeaseHeld)
	renewed, err := m.Renew(ctx, first, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, first.Epoch, renewed.Epoch)
	require.True(t, renewed.ExpiresAt.After(first.ExpiresAt))
	second, err := m.Transfer(ctx, first, b, time.Second)
	require.NoError(t, err)
	require.Greater(t, second.Epoch, first.Epoch)
	_, err = m.Renew(ctx, first, time.Second)
	require.ErrorIs(t, err, ErrLeaseLost)
	_, err = m.Transfer(ctx, first, b, time.Second)
	require.ErrorIs(t, err, ErrLeaseLost)
	require.NoError(t, m.Release(ctx, first))
	current, err := m.Get(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, second, current)
	owner, err := m.Owner(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, b, owner)
	leases, err := m.ListByWorker(ctx, b)
	require.NoError(t, err)
	require.Equal(t, []Lease{second}, leases)
	leases, err = m.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Empty(t, leases)
	third, err := m.Transfer(ctx, second, a, time.Second)
	require.NoError(t, err)
	require.NoError(t, m.Release(ctx, first), "stale lease with same address")
	require.NoError(t, m.Release(ctx, third))
	fourth, err := m.Claim(ctx, "s", a, time.Second)
	require.NoError(t, err)
	require.Greater(t, fourth.Epoch, third.Epoch, "epochs survive release")
	_, err = m.Claim(ctx, "", a, time.Second)
	require.Error(t, err)
	_, err = m.Claim(ctx, "bad", netip.AddrPort{}, time.Second)
	require.Error(t, err)
	_, err = m.Renew(ctx, fourth, 0)
	require.Error(t, err)
}

func TestConcurrentTransfersExactlyOneWins(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	lease, err := m.Claim(ctx, "s", a, time.Minute)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			_, err := m.Transfer(ctx, lease, b, time.Minute)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrLeaseLost)
		}
	}
	require.Equal(t, 1, wins)
}

func TestExpiredLeaseDoesNotTakeOver(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	lease, err := m.Claim(ctx, "s", a, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = m.Renew(ctx, lease, time.Second)
	require.ErrorIs(t, err, ErrLeaseLost)
	_, err = m.Transfer(ctx, lease, b, time.Second)
	require.ErrorIs(t, err, ErrLeaseLost)
	_, err = m.Claim(ctx, "s", b, time.Second)
	require.ErrorIs(t, err, ErrLeaseHeld)
	_, err = m.Owner(ctx, "s")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGlobalEpochAndExpiredLeasePruning(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	first, err := m.Claim(ctx, "first", a, time.Millisecond)
	require.NoError(t, err)
	second, err := m.Claim(ctx, "second", a, time.Minute)
	require.NoError(t, err)
	require.Greater(t, second.Epoch, first.Epoch, "one global epoch without session history")
	time.Sleep(5 * time.Millisecond)
	_, err = m.Get(ctx, "first")
	require.ErrorIs(t, err, ErrNotFound)
	require.Len(t, m.leases, 1, "expired record pruned on lookup")
	reclaimed, err := m.Claim(ctx, "first", a, time.Minute)
	require.NoError(t, err)
	require.Greater(t, reclaimed.Epoch, second.Epoch)
	_, err = m.Renew(ctx, first, time.Minute)
	require.ErrorIs(t, err, ErrLeaseLost)
	require.NoError(t, m.Release(ctx, first))
	current, err := m.Get(ctx, "first")
	require.NoError(t, err)
	require.Equal(t, reclaimed, current, "stale release cannot remove reclaimed lease")
	require.NoError(t, m.Release(ctx, reclaimed))
	require.NoError(t, m.Release(ctx, second))
	require.Empty(t, m.leases)
}

func TestListPrunesExpiredLeases(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	b := netip.MustParseAddrPort("127.0.0.1:2")
	_, err := m.Claim(ctx, "expired", a, time.Millisecond)
	require.NoError(t, err)
	live, err := m.Claim(ctx, "live", b, time.Minute)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	leases, err := m.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Empty(t, leases)
	require.Len(t, m.leases, 1, "listing lazily removes expired records")
	leases, err = m.ListByWorker(ctx, b)
	require.NoError(t, err)
	require.Equal(t, []Lease{live}, leases)
}
