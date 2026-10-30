package sessionstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelaySlotsKeepIndependentRoutes(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		routes := s.(Routes)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-route", netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		pending := RelayAssignment{Slot: "r1", Pending: "r2"}
		require.NoError(t, a.CASRelay(ctx, lease, RelayAssignment{}, current))
		require.NoError(t, a.CASRelay(ctx, lease, current, pending))
		r1 := routeFor(lease, netip.MustParseAddrPort("192.0.2.4:1234"), time.Minute)
		r1.Slot = "r1"
		r2 := r1
		r2.Slot = "r2"
		for _, route := range []Route{r1, r2} {
			require.NoError(t, routes.PutRoute(ctx, route, true))
		}
		got, err := routes.LoadRoutes(ctx, 20)
		require.NoError(t, err)
		require.ElementsMatch(t, []Route{r1, r2}, got, "same caller tuple on different slots must remain distinct")
		// A nomination on one slot must not delete the other's confirmed route.
		r2.Caller = netip.MustParseAddrPort("192.0.2.4:5678")
		r2.ConfirmedAt = r2.ConfirmedAt.Add(time.Millisecond)
		r2.LastAuthenticated = r2.ConfirmedAt
		require.NoError(t, routes.PutRoute(ctx, r2, true))
		got, err = routes.LoadRoutes(ctx, 20)
		require.NoError(t, err)
		require.ElementsMatch(t, []Route{r1, r2}, got)
		other := r2
		other.Slot = "r3"
		require.ErrorIs(t, routes.PutRoute(ctx, other, true), ErrLeaseLost)
		require.NoError(t, routes.DeleteRoute(ctx, r1))
		got, err = routes.LoadRoutes(ctx, 20)
		require.NoError(t, err)
		require.Equal(t, []Route{r2}, got)
		require.NoError(t, a.CASRelay(ctx, lease, pending, RelayAssignment{Slot: "r2", Generation: 1}))
		require.ErrorIs(t, routes.PutRoute(ctx, r1, false), ErrLeaseLost, "retired slot cannot persist new evidence")
	})
}

func TestRelayAssignmentExpiryAndCanceledWrite(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		a := relayAssignments(t, s)
		ctx := context.Background()
		lease, err := s.Claim(ctx, "relay-expiry", netip.MustParseAddrPort("127.0.0.1:17001"), 500*time.Millisecond)
		require.NoError(t, err)
		current := RelayAssignment{Slot: "r1"}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		require.ErrorIs(t, a.CASRelay(canceled, lease, RelayAssignment{}, current), context.Canceled)
		require.NoError(t, a.CASRelay(ctx, lease, RelayAssignment{}, current))
		require.Eventually(t, func() bool { _, _, err := a.RelayOwner(ctx, lease.SessionID); return err == ErrNotFound }, 3*time.Second, 10*time.Millisecond)
		require.ErrorIs(t, a.CASRelay(ctx, lease, current, RelayAssignment{Slot: "r1", Pending: "r2"}), ErrLeaseLost)
	})
}
