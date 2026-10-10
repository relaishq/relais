package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func routeFor(lease Lease, caller netip.AddrPort, window time.Duration) Route {
	now := time.Now().UTC()
	return Route{Caller: caller, SessionID: lease.SessionID, Generation: lease.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(window)}
}
func TestPersistedRoutesBothStores(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		owner := netip.MustParseAddrPort("127.0.0.1:4101")
		successor := netip.MustParseAddrPort("127.0.0.1:4102")
		caller := netip.MustParseAddrPort("127.0.0.1:5101")
		other := netip.MustParseAddrPort("127.0.0.1:5102")
		lease, err := store.Claim(ctx, "route-session", owner, time.Minute)
		require.NoError(t, err)
		original := routeFor(lease, caller, 30*time.Second)
		require.NoError(t, routes.PutRoute(ctx, original, false))
		loaded, err := routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []Route{original}, loaded)

		// Renewal is monotonic. Stale cleanup and a delayed write preserve it.
		renewed := original
		renewed.LastAuthenticated = renewed.LastAuthenticated.Add(time.Millisecond)
		renewed.ExpiresAt = renewed.ExpiresAt.Add(time.Millisecond)
		require.NoError(t, routes.PutRoute(ctx, renewed, false))
		require.NoError(t, routes.PutRoute(ctx, original, false))
		require.NoError(t, routes.DeleteRoute(ctx, original))
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []Route{renewed}, loaded)

		// A nomination invalidates the previous address; delayed packets cannot
		// republish it, even when the old field has already been removed.
		nominated := renewed
		nominated.Caller = other
		nominated.ConfirmedAt = nominated.ConfirmedAt.Add(2 * time.Millisecond)
		nominated.LastAuthenticated = nominated.LastAuthenticated.Add(2 * time.Millisecond)
		nominated.ExpiresAt = nominated.ExpiresAt.Add(2 * time.Millisecond)
		require.NoError(t, routes.PutRoute(ctx, nominated, true))
		require.NoError(t, routes.PutRoute(ctx, renewed, false))
		require.NoError(t, routes.PutRoute(ctx, original, true))
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []Route{nominated}, loaded)

		next, err := store.Transfer(ctx, lease, successor, time.Minute)
		require.NoError(t, err)
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, loaded, "new generation invalidates old route immediately")
		require.ErrorIs(t, routes.PutRoute(ctx, nominated, false), ErrLeaseLost)
		moved := nominated
		moved.Generation = next.Epoch
		require.NoError(t, routes.PutRoute(ctx, moved, false))
		require.NoError(t, store.Release(ctx, lease), "stale release must preserve successor")
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []Route{moved}, loaded)
		require.Equal(t, nominated.LastAuthenticated, loaded[0].LastAuthenticated, "move never renews consent")
		require.NoError(t, routes.ForgetRoutes(ctx, lease.SessionID))
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, loaded)
		require.NoError(t, routes.PutRoute(ctx, moved, false))
		require.NoError(t, store.Release(ctx, next))
		loaded, err = routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, loaded, "hangup removes persisted records")
		require.ErrorIs(t, routes.PutRoute(ctx, moved, false), ErrLeaseLost)
	})
}

func TestPersistedRouteExpiryUsesConsentNotSessionTTL(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		lease, err := store.Claim(ctx, "expiry", netip.MustParseAddrPort("127.0.0.1:4201"), time.Minute)
		require.NoError(t, err)
		route := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:5201"), 80*time.Millisecond)
		require.NoError(t, routes.PutRoute(ctx, route, false))
		if redis, ok := store.(*Redis); ok {
			ttl, err := redis.client.PTTL(ctx, redis.keys(lease.SessionID)[4]).Result()
			require.NoError(t, err)
			require.LessOrEqual(t, ttl, 80*time.Millisecond+RouteRetention)
			raw, err := redis.client.HGet(ctx, redis.keys(lease.SessionID)[4], route.Caller.String()).Result()
			require.NoError(t, err)
			require.False(t, strings.Contains(raw, lease.Worker.String()), "worker address is never persisted in a route")
			require.NoError(t, redis.client.HSet(ctx, redis.keys(lease.SessionID)[4], route.Caller.String(), raw).Err())
		}
		time.Sleep(100 * time.Millisecond)
		_, err = store.Get(ctx, lease.SessionID)
		require.NoError(t, err, "session lease remains live")
		loaded, err := routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, loaded)
		require.ErrorIs(t, routes.PutRoute(ctx, route, false), ErrNotFound)
	})
}
func TestPersistedRouteLimitsAndLeaseExpiry(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		owner := netip.MustParseAddrPort("127.0.0.1:4301")
		lease, err := store.Claim(ctx, "limits", owner, time.Minute)
		require.NoError(t, err)
		for _, addr := range []string{"127.0.0.1:5301", "127.0.0.1:5302"} {
			require.NoError(t, routes.PutRoute(ctx, routeFor(lease, netip.MustParseAddrPort(addr), time.Second), false))
		}
		_, err = routes.LoadRoutes(ctx, 1)
		require.ErrorIs(t, err, ErrRouteLimit)
		require.NoError(t, routes.ForgetRoutes(ctx, lease.SessionID))
		short, err := store.Claim(ctx, "lease-expiry", owner, 60*time.Millisecond)
		require.NoError(t, err)
		require.NoError(t, routes.PutRoute(ctx, routeFor(short, netip.MustParseAddrPort("127.0.0.1:5401"), time.Second), false))
		time.Sleep(90 * time.Millisecond)
		loaded, err := routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, loaded, "a record without a live owner is ignored and removed")
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = routes.LoadRoutes(canceled, 10)
		require.Error(t, err)
	})
}
func TestKeylessRelayCanPersistRoutesWithoutSnapshotAccess(t *testing.T) {
	redis := testRedis(t)
	ctx := context.Background()
	owner, err := NewRedisOwners(ctx, storage.RedisConfig{Addr: os.Getenv("RELAIS_TEST_REDIS_ADDR"), Prefix: redis.prefix})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	lease, err := redis.Claim(ctx, "keyless", netip.MustParseAddrPort("127.0.0.1:4501"), time.Minute)
	require.NoError(t, err)
	route := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:5501"), time.Second)
	require.NoError(t, owner.PutRoute(ctx, route, true))
	loaded, err := owner.LoadRoutes(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, []Route{route}, loaded)
	raw, err := json.Marshal(route)
	require.NoError(t, err)
	require.NotContains(t, string(raw), lease.Worker.String())
	require.Nil(t, owner.leases.keysByID)
}

func TestAddressReuseDoesNotCompareDifferentSessionsEpochs(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		owner := netip.MustParseAddrPort("127.0.0.1:4601")
		caller := netip.MustParseAddrPort("127.0.0.1:5601")
		earlier, err := store.Claim(ctx, "earlier-epoch", owner, time.Minute)
		require.NoError(t, err)
		later, err := store.Claim(ctx, "later-epoch", owner, time.Minute)
		require.NoError(t, err)
		old := routeFor(later, caller, 50*time.Millisecond)
		require.NoError(t, routes.PutRoute(ctx, old, false))
		time.Sleep(70 * time.Millisecond)
		current := routeFor(earlier, caller, time.Second)
		require.NoError(t, routes.PutRoute(ctx, current, false))
		loaded, err := routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []Route{current}, loaded, "a session with a lower epoch can claim a quiet address")
	})
}

// Inject legacy debris directly: old releases did not prune fields on renewal.
func injectExpiredRoutes(t *testing.T, store Store, lease Lease, count int) {
	t.Helper()
	ctx := context.Background()
	var pipe redis.Pipeliner
	if r, ok := store.(*Redis); ok {
		pipe = r.client.Pipeline()
	}
	for i := 0; i < count; i++ {
		route := routeFor(lease, netip.AddrPortFrom(netip.MustParseAddr("::1"), uint16(10000+i)), time.Second)
		route.LastAuthenticated = time.Now().UTC().Add(-time.Minute)
		route.ConfirmedAt = route.LastAuthenticated
		route.ExpiresAt = route.LastAuthenticated.Add(time.Second)
		switch s := store.(type) {
		case *Memory:
			s.mu.Lock()
			s.routes[route.Caller] = route
			if s.routeSessions[lease.SessionID] == nil {
				s.routeSessions[lease.SessionID] = make(map[netip.AddrPort]struct{})
			}
			s.routeSessions[lease.SessionID][route.Caller] = struct{}{}
			s.mu.Unlock()
		case *Redis:
			raw, err := routeJSON(route)
			require.NoError(t, err)
			pipe.HSet(ctx, s.keys(lease.SessionID)[4], route.Caller.String(), raw)
		}
	}
	if pipe != nil {
		_, err := pipe.Exec(ctx)
		require.NoError(t, err)
	}
}
func TestExpiredRouteFieldsCannotPoisonRestoreBothStores(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		lease, err := store.Claim(ctx, "expired-debris", netip.MustParseAddrPort("127.0.0.1:4801"), time.Minute)
		require.NoError(t, err)
		live := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:5801"), 30*time.Second)
		require.NoError(t, routes.PutRoute(ctx, live, false))
		injectExpiredRoutes(t, store, lease, 8000)
		loaded, err := routes.LoadRoutes(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, []Route{live}, loaded)
		injectExpiredRoutes(t, store, lease, 8000)
		live.LastAuthenticated = live.LastAuthenticated.Add(time.Millisecond)
		live.ExpiresAt = live.ExpiresAt.Add(time.Millisecond)
		require.NoError(t, routes.PutRoute(ctx, live, false))
		if s, ok := store.(*Redis); ok {
			n, err := s.client.HLen(ctx, s.keys(lease.SessionID)[4]).Result()
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
		}
		loaded, err = routes.LoadRoutes(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, []Route{live}, loaded)
	})
}
func TestRouteAddressChurnIsBoundedBothStores(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		lease, err := store.Claim(ctx, "churn", netip.MustParseAddrPort("127.0.0.1:4802"), time.Minute)
		require.NoError(t, err)
		var last Route
		for i := 0; i < 100; i++ {
			last = routeFor(lease, netip.AddrPortFrom(netip.MustParseAddr("::1"), uint16(20000+i)), time.Minute)
			require.NoError(t, routes.PutRoute(ctx, last, false))
		}
		loaded, err := routes.LoadRoutes(ctx, 100)
		require.NoError(t, err)
		require.Len(t, loaded, MaxRoutesPerSession)
		require.Contains(t, loaded, last)
		delayed := last
		delayed.Caller = netip.MustParseAddrPort("[::1]:29999")
		delayed.LastAuthenticated = last.LastAuthenticated.Add(-time.Second)
		delayed.ConfirmedAt = delayed.LastAuthenticated
		require.NoError(t, routes.PutRoute(ctx, delayed, false))
		loaded, err = routes.LoadRoutes(ctx, 100)
		require.NoError(t, err)
		require.Len(t, loaded, MaxRoutesPerSession)
		require.NotContains(t, loaded, delayed)
		for _, route := range loaded {
			require.GreaterOrEqual(t, int(route.Caller.Port()), 20092)
		}
	})
}
func TestSamePairNominationPreservesBackupBothStores(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		ctx := context.Background()
		store := factory(t)
		routes := store.(Routes)
		lease, err := store.Claim(ctx, "backup", netip.MustParseAddrPort("127.0.0.1:4803"), time.Minute)
		require.NoError(t, err)
		primary := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:5803"), time.Minute)
		require.NoError(t, routes.PutRoute(ctx, primary, true))
		backup := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:5804"), time.Minute)
		require.NoError(t, routes.PutRoute(ctx, backup, false))
		primary.LastAuthenticated = backup.LastAuthenticated.Add(time.Millisecond)
		primary.ExpiresAt = primary.LastAuthenticated.Add(time.Minute)
		require.NoError(t, routes.PutRoute(ctx, primary, true))
		loaded, err := routes.LoadRoutes(ctx, 10)
		require.NoError(t, err)
		require.ElementsMatch(t, []Route{primary, backup}, loaded)
	})
}

// Drop one real pipelined lease reply while leaving the other session readable.
type failedRouteLeaseHook struct {
	lostResponseHook
	leaseKey string
	err      error
}

func (h *failedRouteLeaseHook) AfterProcessPipeline(_ context.Context, cmds []redis.Cmder) error {
	for _, cmd := range cmds {
		args := sessionCommandArgs(cmd)
		wire := cmd.Args()
		if cmd.Name() == "eval" && len(wire) > 3 && wire[3] == h.leaseKey && len(args) > 0 && args[0] == "get" {
			cmd.SetErr(h.err)
		}
	}
	return nil
}

func TestRedisRouteLoaderRetainsAddressWithFailedNewestLease(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	caller := netip.MustParseAddrPort("127.0.0.1:5911")
	olderLease, err := r.Claim(ctx, "older", netip.MustParseAddrPort("127.0.0.1:4911"), time.Minute)
	require.NoError(t, err)
	newerLease, err := r.Claim(ctx, "newer", netip.MustParseAddrPort("127.0.0.1:4912"), time.Minute)
	require.NoError(t, err)
	older := routeFor(olderLease, caller, time.Minute)
	newer := older
	newer.SessionID, newer.Generation = newerLease.SessionID, newerLease.Epoch
	newer.LastAuthenticated = older.LastAuthenticated.Add(time.Millisecond)
	for _, route := range []Route{older, newer} {
		require.NoError(t, r.PutRoute(ctx, route, false))
	}
	fault := errors.New("newest lease read failed")
	r.client.AddHook(&failedRouteLeaseHook{leaseKey: r.keys(newer.SessionID)[0], err: fault})
	loaded, owners, err := r.LoadRouteOwners(ctx, 10)
	require.ErrorIs(t, err, fault)
	require.ElementsMatch(t, []Route{older, newer}, loaded, "failed lease evidence must still reserve its caller address")
	require.Equal(t, olderLease, owners[older.SessionID])
	_, known := owners[newer.SessionID]
	require.False(t, known, "a failed lease read must not become an authoritative missing lease")
	loaded, owners, err = r.LoadRouteOwners(ctx, 1)
	require.ErrorIs(t, err, fault)
	require.ErrorIs(t, err, ErrRouteLimit)
	require.Equal(t, []Route{newer}, loaded, "a full batch must still reserve a loaded address with its newest evidence")
	_, known = owners[newer.SessionID]
	require.False(t, known)
	raw, err := r.client.HGet(ctx, r.keys(newer.SessionID)[4], caller.String()).Result()
	require.NoError(t, err)
	require.NotEmpty(t, raw, "a transient read failure must not delete the newest evidence")
}
