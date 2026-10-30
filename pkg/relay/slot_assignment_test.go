package relay

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestSlotAdmitsOnlyCurrentOrPendingBinding(t *testing.T) {
	store := sessionstore.NewMemory()
	r1 := startTestRelay(t, Config{Owners: store, SlotID: "r1"})
	r2 := startTestRelay(t, Config{Owners: store, SlotID: "r2"})
	r3 := startTestRelay(t, Config{Owners: store, SlotID: "r3"})
	w := r1.worker(t, sessionA)
	r2.relay.AddWorker(w.addr())
	r3.relay.AddWorker(w.addr())
	ctx := context.Background()
	lease, err := store.Get(ctx, sessionA)
	require.NoError(t, err)
	current := sessionstore.RelayAssignment{Slot: "r1"}
	pending := sessionstore.RelayAssignment{Slot: "r1", Pending: "r2"}
	require.NoError(t, store.CASRelay(ctx, lease, sessionstore.RelayAssignment{}, current))
	require.NoError(t, store.CASRelay(ctx, lease, current, pending))
	c := newTestCaller(t)
	for _, sys := range []*testSystem{r1, r2} {
		request := bindingRequest(t, sessionA+"+1")
		c.send(t, request, sys.relay.PublicAddr())
		w.expect(t, c.addr(), request)
		sys.confirm(t, w, c, request)
	}
	request := bindingRequest(t, sessionA+"+1")
	c.send(t, request, r3.relay.PublicAddr())
	require.Eventually(t, func() bool { return r3.relay.Stats().UnknownSession > 0 }, receiveTimeout, time.Millisecond)
	w.expectNothing(t)
	// A cached confirmed route must not bypass the assignment lookup.
	require.NoError(t, store.CASRelay(ctx, lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
	c.send(t, bindingRequest(t, sessionA+"+2"), r1.relay.PublicAddr())
	require.Eventually(t, func() bool { return r1.relay.Stats().UnknownSession > 0 }, receiveTimeout, time.Millisecond)
	w.expectNothing(t)
}

func TestSlotRestoresOnlyItsAssignedRoutes(t *testing.T) {
	store := sessionstore.NewMemory()
	ctx := context.Background()
	r1 := startTestRelay(t, Config{Owners: store, Routes: store, SlotID: "r1"})
	w := r1.worker(t, sessionA)
	lease, err := store.Get(ctx, sessionA)
	require.NoError(t, err)
	current := sessionstore.RelayAssignment{Slot: "r1"}
	pending := sessionstore.RelayAssignment{Slot: "r1", Pending: "r2"}
	require.NoError(t, store.CASRelay(ctx, lease, sessionstore.RelayAssignment{}, current))
	require.NoError(t, store.CASRelay(ctx, lease, current, pending))
	c := newTestCaller(t)
	r1.connect(t, c, w, sessionA)
	require.Eventually(t, func() bool { return r1.relay.Stats().RouteWrites > 0 }, receiveTimeout, time.Millisecond)
	r2 := startTestRelay(t, Config{Owners: store, Routes: store, SlotID: "r2"})
	require.Zero(t, r2.relay.Stats().RoutesRestored, "pending slot cannot restore another slot's route")
	r1.restart(t)
	require.EqualValues(t, 1, r1.relay.Stats().RoutesRestored)
	require.NoError(t, store.CASRelay(ctx, lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
	r1.restart(t)
	require.Zero(t, r1.relay.Stats().RoutesRestored, "unassigned slot cannot restore old evidence")
}
