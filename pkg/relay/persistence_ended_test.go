package relay

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestEndedRouteWriteIsNotStoreFailure(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		worker := sys.worker(t, sessionA)
		caller := newTestCaller(t)
		sys.connect(t, caller, worker, sessionA)
		persistedRoute(t, routes, caller.addr(), func(record sessionstore.Route) bool { return true })
		snapshot, ok := sys.relay.flows.snapshot(caller.addr(), true, time.Now())
		require.True(t, ok)
		lease, err := store.Get(context.Background(), sessionA)
		require.NoError(t, err)
		require.NoError(t, store.Release(context.Background(), lease))
		before := sys.relay.Stats().RouteWritesFailed
		sys.relay.persistence.write(snapshot)
		require.Equal(t, before, sys.relay.Stats().RouteWritesFailed)
		require.Contains(t, relayMetricsBody(sys.relay), "relais_relay_route_write_failures_total 0\n")
	})
}

func TestMemoryRestoreRejectsEndedTenure(t *testing.T) {
	store := sessionstore.NewMemory()
	sys := startTestRelay(t, Config{Owners: store, Routes: store})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, worker, sessionA)
	record := persistedRoute(t, store, caller.addr(), func(record sessionstore.Route) bool { return true })
	lease, err := store.Get(context.Background(), sessionA)
	require.NoError(t, err)
	require.NoError(t, store.Release(context.Background(), lease))
	// A captured startup page can race the release. Get must not restore the
	// ended token that happens to retain the route's owner and epoch.
	sys.cfg.Routes = &partialRouteStore{Routes: store, records: []sessionstore.Route{record}}
	sys.restart(t)
	require.Zero(t, sys.relay.Stats().RoutesRestored)
	require.EqualValues(t, 1, sys.relay.Stats().RoutesRestoreSkipped)
	require.Zero(t, sys.relay.Stats().RoutesRestoreFailed)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
}
