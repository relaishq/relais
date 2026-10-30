package sessionstore

import (
	"context"
	"math"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func relayAssignments(t *testing.T, store Store) RelayAssignments {
	t.Helper()
	a, ok := store.(RelayAssignments)
	require.True(t, ok, "store must implement atomic relay assignments")
	return a
}

func TestRelayAssignmentLifecycle(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-assignment", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		require.NoError(t, a.CASRelay(ctx, lease, RelayAssignment{}, current))
		for g := uint64(0); g < 20; g++ {
			pending := current
			pending.Pending = "r2"
			if current.Slot == "r2" {
				pending.Pending = "r1"
			}
			require.NoError(t, a.CASRelay(ctx, lease, current, pending))
			require.ErrorIs(t, a.CASRelay(ctx, lease, current, pending), ErrRelayConflict)
			next := RelayAssignment{Slot: pending.Pending, Generation: g + 1}
			require.NoError(t, a.CASRelay(ctx, lease, pending, next))
			require.ErrorIs(t, a.CASRelay(ctx, lease, pending, next), ErrRelayConflict)
			gotLease, got, err := a.RelayOwner(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, gotLease.Epoch)
			require.Equal(t, next, got)
			current = next
		}
		// A rollback consumes the pending generation and names the old slot.
		pending := current
		pending.Pending = "r3"
		require.NoError(t, a.CASRelay(ctx, lease, current, pending))
		rolledBack := RelayAssignment{Slot: current.Slot, Generation: current.Generation + 1}
		require.NoError(t, a.CASRelay(ctx, lease, pending, rolledBack))
		require.ErrorIs(t, a.CASRelay(ctx, lease, pending, RelayAssignment{Slot: "r3", Generation: rolledBack.Generation}), ErrRelayConflict)
		sameSlot := rolledBack
		sameSlot.Pending = sameSlot.Slot
		require.NoError(t, a.CASRelay(ctx, lease, rolledBack, sameSlot))
		recovered := RelayAssignment{Slot: sameSlot.Slot, Generation: sameSlot.Generation + 1}
		require.NoError(t, a.CASRelay(ctx, lease, sameSlot, recovered))
		require.NoError(t, s.Release(ctx, lease))
		_, _, err = a.RelayOwner(ctx, lease.SessionID)
		require.ErrorIs(t, err, ErrNotFound)
		lease, err = s.Claim(ctx, lease.SessionID, lease.Worker, time.Minute)
		require.NoError(t, err)
		_, got, err := a.RelayOwner(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, RelayAssignment{}, got, "new call must not inherit a released assignment")
	})
}

func TestRelayAssignmentWorkerFence(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		ctx := context.Background()
		old, err := s.Claim(ctx, "relay-worker-fence", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		require.NoError(t, a.CASRelay(ctx, old, RelayAssignment{}, current))
		pending := RelayAssignment{Slot: "r1", Pending: "r2"}
		require.NoError(t, a.CASRelay(ctx, old, current, pending))
		lease, err := s.Transfer(ctx, old, netip.MustParseAddrPort("127.0.0.1:17002"), time.Minute)
		require.NoError(t, err)
		renewed, err := s.Renew(ctx, lease, time.Minute)
		require.NoError(t, err)
		gotLease, got, err := a.RelayOwner(ctx, old.SessionID)
		require.NoError(t, err)
		require.Equal(t, renewed.Epoch, gotLease.Epoch)
		require.Equal(t, pending, got, "worker transfer must preserve assignment")
		next := RelayAssignment{Slot: "r2", Generation: 1}
		require.ErrorIs(t, a.CASRelay(ctx, old, pending, next), ErrLeaseLost)
		require.NoError(t, a.CASRelay(ctx, renewed, pending, next))
	})
}

func TestRelayAssignmentConcurrentBegin(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-race", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		require.NoError(t, a.CASRelay(ctx, lease, RelayAssignment{}, current))
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, slot := range []string{"r2", "r3"} {
			wg.Go(func() { results <- a.CASRelay(ctx, lease, current, RelayAssignment{Slot: "r1", Pending: slot}) })
		}
		wg.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			} else {
				require.ErrorIs(t, err, ErrRelayConflict)
			}
		}
		require.Equal(t, 1, successes)
	})
}

func TestRelayAssignmentRejectsInvalidTransitions(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-invalid", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		for _, tc := range []struct{ from, to RelayAssignment }{
			{RelayAssignment{}, RelayAssignment{Slot: "r1", Generation: 1}},
			{RelayAssignment{Slot: "r1"}, RelayAssignment{Slot: "r2", Generation: 1}},
			{RelayAssignment{Slot: "r1", Pending: "r2"}, RelayAssignment{Slot: "r1"}},
			{RelayAssignment{Slot: "r1", Pending: "r2"}, RelayAssignment{Slot: "r3", Generation: 1}},
			{RelayAssignment{Slot: "r1", Generation: math.MaxUint64}, RelayAssignment{Slot: "r1", Generation: math.MaxUint64, Pending: "r2"}},
		} {
			require.ErrorIs(t, a.CASRelay(ctx, lease, tc.from, tc.to), ErrRelayTransition)
		}
	})
}
