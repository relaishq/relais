package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relais/internal/redisendpoint"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func routeStores(t *testing.T, test func(*testing.T, sessionstore.Store, sessionstore.Routes)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { store := sessionstore.NewMemory(); test(t, store, store) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		require.NoError(t, redisendpoint.Validate(addr))
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "relay-route-test:" + rand.Text() + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		test(t, store, store)
	})
}
func persistedRoute(t *testing.T, routes sessionstore.Routes, caller netip.AddrPort, check func(sessionstore.Route) bool) sessionstore.Route {
	t.Helper()
	var result sessionstore.Route
	require.Eventually(t, func() bool {
		loaded, err := routes.LoadRoutes(context.Background(), 100)
		if err != nil {
			return false
		}
		for _, record := range loaded {
			if record.Caller == caller && check(record) {
				result = record
				return true
			}
		}
		return false
	}, 2*time.Second, time.Millisecond)
	return result
}
func TestPersistedRelayRestoresMediaAndStickinessBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes, RouteStickinessWindow: 2 * time.Second})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		record := persistedRoute(t, routes, caller.addr(), func(record sessionstore.Route) bool { return record.SessionID == sessionA })
		sys.restart(t)
		caller.send(t, media, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), media, "media restores without an ICE check")
		caller.send(t, bindingRequest(t, sessionB), sys.relay.PublicAddr())
		b.expectNothing(t)
		a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
		require.EqualValues(t, 1, sys.relay.Stats().RoutesRestored)
		loaded, err := routes.LoadRoutes(context.Background(), 100)
		require.NoError(t, err)
		require.Equal(t, record.LastAuthenticated, loaded[0].LastAuthenticated, "restore and media never renew stickiness")
		// Only the remainder of the original window is protected.
		time.Sleep(time.Until(record.ExpiresAt) + 10*time.Millisecond)
		check := bindingRequest(t, sessionB)
		caller.send(t, check, sys.relay.PublicAddr())
		b.expect(t, caller.addr(), check, "original consent deadline releases the address")
	})
}
func TestPersistedRelayMoveUsesCurrentGenerationBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		record := persistedRoute(t, routes, caller.addr(), func(record sessionstore.Route) bool { return true })
		lease, err := store.Get(context.Background(), sessionA)
		require.NoError(t, err)
		next, err := store.Transfer(context.Background(), lease, b.addr(), time.Minute)
		require.NoError(t, err)
		require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
		moved := persistedRoute(t, routes, caller.addr(), func(record sessionstore.Route) bool { return record.Generation == next.Epoch })
		require.Equal(t, record.LastAuthenticated, moved.LastAuthenticated)
		require.Equal(t, record.ConfirmedAt, moved.ConfirmedAt)
		sys.restart(t)
		caller.send(t, media, sys.relay.PublicAddr())
		b.expect(t, caller.addr(), media)
		a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expectNothing(t)
		b.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	})
}
func TestPersistedRelayHangupAndNewGenerationInvalidateBothStores(t *testing.T) {
	for _, action := range []string{"hangup", "new-generation", "forget"} {
		t.Run(action, func(t *testing.T) {
			routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
				sys := startTestRelay(t, Config{Owners: store, Routes: routes})
				a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
				caller := newTestCaller(t)
				sys.connect(t, caller, a, sessionA)
				persistedRoute(t, routes, caller.addr(), func(sessionstore.Route) bool { return true })
				lease, err := store.Get(context.Background(), sessionA)
				require.NoError(t, err)
				switch action {
				case "hangup":
					require.NoError(t, store.Release(context.Background(), lease))
					sys.relay.ForgetSession(sessionA)
				case "new-generation":
					_, err = store.Transfer(context.Background(), lease, b.addr(), time.Minute)
					require.NoError(t, err)
				case "forget":
					sys.relay.ForgetSession(sessionA)
				}
				require.Eventually(t, func() bool {
					loaded, err := routes.LoadRoutes(context.Background(), 100)
					return err == nil && len(loaded) == 0
				}, 2*time.Second, time.Millisecond)
				sys.restart(t)
				caller.send(t, media, sys.relay.PublicAddr())
				a.expectNothing(t)
				b.expectNothing(t)
			})
		})
	}
}
func TestPersistedRelayRenominationInvalidatesOldAddressBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		a := sys.worker(t, sessionA)
		old, next := newTestCaller(t), newTestCaller(t)
		sys.connect(t, old, a, sessionA)
		persistedRoute(t, routes, old.addr(), func(sessionstore.Route) bool { return true })
		request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(sessionA+":callerufrag"), stun.RawAttribute{Type: stun.AttrUseCandidate}, stun.NewShortTermIntegrity("worker-ice-password"), stun.Fingerprint)
		require.NoError(t, err)
		next.send(t, request.Raw, sys.relay.PublicAddr())
		a.expect(t, next.addr(), request.Raw)
		sys.confirm(t, a, next, request.Raw)
		persistedRoute(t, routes, next.addr(), func(sessionstore.Route) bool { return true })
		sys.restart(t)
		old.send(t, media, sys.relay.PublicAddr())
		a.expectNothing(t)
		next.send(t, media, sys.relay.PublicAddr())
		a.expect(t, next.addr(), media)
	})
}
func TestRestoredRouteExpiresDespiteMediaBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes, RouteStickinessWindow: 150 * time.Millisecond})
		a := sys.worker(t, sessionA)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		record := persistedRoute(t, routes, caller.addr(), func(sessionstore.Route) bool { return true })
		sys.restart(t)
		caller.send(t, media, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), media)
		time.Sleep(time.Until(record.ExpiresAt) + 10*time.Millisecond)
		caller.send(t, media, sys.relay.PublicAddr())
		a.expectNothing(t)
	})
}
func TestRouteRenewalPersistenceIsThrottled(t *testing.T) {
	store := sessionstore.NewMemory()
	sys := startTestRelay(t, Config{Owners: store, Routes: store})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	original := persistedRoute(t, store, caller.addr(), func(sessionstore.Route) bool { return true })
	for range 20 {
		sys.connect(t, caller, a, sessionA)
	}
	require.EqualValues(t, 1, sys.relay.Stats().RouteWrites)
	time.Sleep(time.Second)
	sys.connect(t, caller, a, sessionA)
	renewed := persistedRoute(t, store, caller.addr(), func(record sessionstore.Route) bool {
		return record.LastAuthenticated.After(original.LastAuthenticated)
	})
	require.Equal(t, original.ConfirmedAt, renewed.ConfirmedAt)
	require.EqualValues(t, 2, sys.relay.Stats().RouteWrites)
}

type blockedRouteStore struct {
	*sessionstore.Memory
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedRouteStore) PutRoute(ctx context.Context, record sessionstore.Route, nominated bool) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.Memory.PutRoute(ctx, record, nominated)
}
func TestRouteWritesAreBoundedAndNeverBlockMedia(t *testing.T) {
	store := &blockedRouteStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	sys := startTestRelay(t, Config{Owners: store, Routes: store, MaxQueuedLookups: 2, OwnerLookupTimeout: 2 * time.Second})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	<-store.entered
	caller.send(t, media, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), media, "slow persistence cannot stop forwarding")
	for range 8 {
		sys.connect(t, newTestCaller(t), a, sessionA)
	}
	require.Positive(t, sys.relay.Stats().RouteWritesDropped, "bounded route overflow is counted")
	close(store.release)
	require.Eventually(t, func() bool { return sys.relay.Stats().RouteWrites > 0 }, 2*time.Second, time.Millisecond)
}
func TestRouteRestoreLimitStartsWithPartialRoutes(t *testing.T) {
	store := sessionstore.NewMemory()
	owner := netip.MustParseAddrPort("127.0.0.1:6100")
	lease, err := store.Claim(context.Background(), sessionA, owner, time.Minute)
	require.NoError(t, err)
	now := time.Now()
	for _, port := range []uint16{6101, 6102} {
		record := sessionstore.Route{Caller: netip.AddrPortFrom(owner.Addr(), port), SessionID: sessionA, Generation: lease.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(time.Second)}
		require.NoError(t, store.PutRoute(context.Background(), record, false))
	}
	r, err := New(Config{Owners: store, Routes: store, MaxFlows: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	require.EqualValues(t, 1, r.Stats().RoutesRestored)
	require.EqualValues(t, 1, r.Stats().RoutesRestoreFailed)
}

type countedRouteStore struct {
	*sessionstore.Memory
	scans atomic.Uint64
}

func (s *countedRouteStore) LoadRoutes(ctx context.Context, limit int) ([]sessionstore.Route, error) {
	s.scans.Add(1)
	return s.Memory.LoadRoutes(ctx, limit)
}
func TestUnknownSourceFloodCannotTriggerRestoreStoreWork(t *testing.T) {
	store := &countedRouteStore{Memory: sessionstore.NewMemory()}
	sys := startTestRelay(t, Config{Owners: store, Routes: store})
	callers := make([]*testCaller, 20)
	for i := range callers {
		callers[i] = newTestCaller(t)
		for range 10 {
			callers[i].send(t, media, sys.relay.PublicAddr())
		}
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().Unroutable >= 200 }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, store.scans.Load(), "only the eager startup scan may load persisted routes")
	callers[0].expectNothing(t)
}

func TestRenominationReusedTransactionUsesOnlyCurrentEvidence(t *testing.T) {
	table := newFlowTable(flowLimits{stickinessWindow: time.Minute, idleTimeout: time.Minute, pendingTimeout: time.Minute, maxFlows: 10, maxPending: 10})
	worker := netip.MustParseAddrPort("127.0.0.1:7100")
	old := netip.MustParseAddrPort("127.0.0.1:7101")
	next := netip.MustParseAddrPort("127.0.0.1:7102")
	now := time.Now()
	var reused []byte
	for _, caller := range []netip.AddrPort{old, next} {
		request := bindingRequest(t, sessionA)
		_, tx, ok := parseBindingRequest(request)
		require.True(t, ok)
		require.True(t, table.admit(caller, worker, sessionA, tx, now))
		allowed, _ := table.answer(caller, worker, bindingSuccess(t, request), now)
		require.True(t, allowed)
		reused = request
	}
	_, tx, _ := parseBindingRequest(reused)
	_, ok := table.routeSTUN(next, sessionA, tx, now.Add(time.Millisecond))
	require.True(t, ok)
	table.markNomination(next, tx)
	allowed, _ := table.answer(next, worker, bindingSuccess(t, reused), now.Add(2*time.Millisecond))
	require.True(t, allowed)
	_, ok = table.route(old, now.Add(3*time.Millisecond))
	require.True(t, ok, "same pair nomination preserves backup paths even with a reused transaction ID")
}

func TestHeldSamePairNominationPreservesBackupAfterMove(t *testing.T) {
	store := sessionstore.NewMemory()
	sys := startTestRelay(t, Config{Owners: store, Routes: store})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	old, next := newTestCaller(t), newTestCaller(t)
	sys.connect(t, old, a, sessionA)
	sys.connect(t, next, a, sessionA)
	beginHold(t, sys, a, sessionA)
	request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(sessionA+":callerufrag"), stun.RawAttribute{Type: stun.AttrUseCandidate}, stun.NewShortTermIntegrity("worker-ice-password"), stun.Fingerprint)
	require.NoError(t, err)
	next.send(t, request.Raw, sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, time.Second, time.Millisecond)
	transferTestLease(t, store, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	_, err = sys.relay.ReleaseSession(sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, next.addr(), request.Raw)
	sys.confirm(t, b, next, request.Raw)
	old.send(t, media, sys.relay.PublicAddr())
	b.expect(t, old.addr(), media)
	require.Eventually(t, func() bool {
		records, err := store.LoadRoutes(context.Background(), 10)
		return err == nil && len(records) == 2
	}, time.Second, time.Millisecond)
	sys.restart(t)
	old.send(t, media, sys.relay.PublicAddr())
	b.expect(t, old.addr(), media)
	next.send(t, media, sys.relay.PublicAddr())
	b.expect(t, next.addr(), media)
}

func TestLivePersistedRouteKeepsMediaAfterConsentWindowBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes, RouteStickinessWindow: 80 * time.Millisecond})
		worker := sys.worker(t, sessionA)
		caller := newTestCaller(t)
		sys.connect(t, caller, worker, sessionA)
		record := persistedRoute(t, routes, caller.addr(), func(sessionstore.Route) bool { return true })
		time.Sleep(time.Until(record.ExpiresAt) + 20*time.Millisecond)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
	})
}
func TestRestoredRouteRenewalRemovesTemporaryConsentBoundBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes, RouteStickinessWindow: 150 * time.Millisecond})
		worker := sys.worker(t, sessionA)
		caller := newTestCaller(t)
		sys.connect(t, caller, worker, sessionA)
		persistedRoute(t, routes, caller.addr(), func(sessionstore.Route) bool { return true })
		sys.restart(t)
		sys.connect(t, caller, worker, sessionA)
		time.Sleep(180 * time.Millisecond)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
	})
}
func TestRepeatedSamePairNominationIsThrottledBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		worker := sys.worker(t, sessionA)
		primary, backup := newTestCaller(t), newTestCaller(t)
		sys.connect(t, primary, worker, sessionA)
		sys.connect(t, backup, worker, sessionA)
		original := persistedRoute(t, routes, primary.addr(), func(sessionstore.Route) bool { return true })
		persistedRoute(t, routes, backup.addr(), func(sessionstore.Route) bool { return true })
		for range 10 {
			request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(sessionA+":callerufrag"), stun.RawAttribute{Type: stun.AttrUseCandidate})
			require.NoError(t, err)
			primary.send(t, request.Raw, sys.relay.PublicAddr())
			worker.expect(t, primary.addr(), request.Raw)
			sys.confirm(t, worker, primary, request.Raw)
		}
		loaded, err := routes.LoadRoutes(context.Background(), 10)
		require.NoError(t, err)
		require.Len(t, loaded, 2)
		require.Contains(t, loaded, original)
		require.EqualValues(t, 2, sys.relay.Stats().RouteWrites)
		backup.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, backup.addr(), media)
	})
}

type partialRouteStore struct {
	sessionstore.Routes
	records []sessionstore.Route
	err     error
	wait    bool
}

func (s *partialRouteStore) LoadRoutes(ctx context.Context, _ int) ([]sessionstore.Route, error) {
	if s.wait {
		<-ctx.Done()
		return s.records, ctx.Err()
	}
	return s.records, s.err
}
func TestRestoreStoreErrorAndDeadlineStillServePackets(t *testing.T) {
	for _, mode := range []string{"error", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			store := sessionstore.NewMemory()
			routes := &partialRouteStore{Routes: store, err: errors.New("master unavailable"), wait: mode == "deadline"}
			sys := startTestRelay(t, Config{Owners: store, Routes: routes, RouteRestoreTimeout: 10 * time.Millisecond})
			require.EqualValues(t, 1, sys.relay.Stats().RoutesRestoreFailed)
			worker := sys.worker(t, sessionA)
			caller := newTestCaller(t)
			sys.connect(t, caller, worker, sessionA)
			caller.send(t, media, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), media)
		})
	}
}
func TestRestoreDuplicateAddressNewestWinsBothOrdersBothStores(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
				sys := startTestRelay(t, Config{Owners: store})
				a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
				caller := newTestCaller(t)
				leaseA, err := store.Get(context.Background(), sessionA)
				require.NoError(t, err)
				leaseB, err := store.Get(context.Background(), sessionB)
				require.NoError(t, err)
				now := time.Now().UTC()
				old := sessionstore.Route{Caller: caller.addr(), SessionID: sessionA, Generation: leaseA.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(time.Minute)}
				winner := old
				winner.SessionID = sessionB
				winner.Generation = leaseB.Epoch
				winner.LastAuthenticated = now.Add(time.Millisecond)
				// Confirmation time is deliberately older: authentication wins first.
				winner.ConfirmedAt = now.Add(-time.Second)
				records := []sessionstore.Route{old, winner}
				if reverse {
					records[0], records[1] = records[1], records[0]
				}
				for _, record := range records {
					require.NoError(t, routes.PutRoute(context.Background(), record, false))
				}
				// Force either scan order at the relay seam; backing-store deletion remains real.
				sys.cfg.Routes = &partialRouteStore{Routes: routes, records: records}
				sys.restart(t)
				caller.send(t, media, sys.relay.PublicAddr())
				b.expect(t, caller.addr(), media)
				a.expectNothing(t)
				loaded, err := routes.LoadRoutes(context.Background(), 10)
				require.NoError(t, err)
				require.Equal(t, []sessionstore.Route{winner}, loaded)
				require.EqualValues(t, 1, sys.relay.Stats().RoutesRestored)
				require.EqualValues(t, 1, sys.relay.Stats().RoutesRestoreSkipped)
			})
		})
	}
}

// A timed-out master must not discard evidence already read from another.
type partialOwnedRouteStore struct {
	sessionstore.Routes
	record sessionstore.Route
	lease  sessionstore.Lease
	wait   bool
}

func (s *partialOwnedRouteStore) LoadRouteOwners(ctx context.Context, _ int) ([]sessionstore.Route, map[string]sessionstore.Lease, error) {
	err := errors.New("one master unavailable")
	if s.wait {
		<-ctx.Done()
		err = ctx.Err()
	}
	return []sessionstore.Route{s.record}, map[string]sessionstore.Lease{s.lease.SessionID: s.lease}, err
}
func TestRestoreErrorAndDeadlineRetainAlreadyLoadedEvidence(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(fmt.Sprint(wait), func(t *testing.T) {
			store := sessionstore.NewMemory()
			sys := startTestRelay(t, Config{Owners: store})
			worker := sys.worker(t, sessionA)
			caller := newTestCaller(t)
			lease, err := store.Get(context.Background(), sessionA)
			require.NoError(t, err)
			now := time.Now().UTC()
			record := sessionstore.Route{Caller: caller.addr(), SessionID: sessionA, Generation: lease.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(time.Minute)}
			require.NoError(t, store.PutRoute(context.Background(), record, false))
			sys.cfg.Routes = &partialOwnedRouteStore{Routes: store, record: record, lease: lease, wait: wait}
			sys.cfg.RouteRestoreTimeout = 10 * time.Millisecond
			sys.restart(t)
			require.EqualValues(t, 1, sys.relay.Stats().RoutesRestoreFailed)
			require.EqualValues(t, 1, sys.relay.Stats().RoutesRestored)
			caller.send(t, media, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), media)
			loaded, err := store.LoadRoutes(context.Background(), 10)
			require.NoError(t, err)
			require.Equal(t, []sessionstore.Route{record}, loaded)
		})
	}
}

// The load seam can report both records even when Memory normally coalesces
// them. Lease reads and record cleanup still use the selected backing store.
type failedLeaseRouteStore struct {
	partialRouteStore
	failedSession string
}

func (s *failedLeaseRouteStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if id == s.failedSession {
		return sessionstore.Lease{}, errors.New("newest lease unavailable")
	}
	return s.Routes.Get(ctx, id)
}

type failedOwnedRouteStore struct {
	*failedLeaseRouteStore
}

func (s *failedOwnedRouteStore) LoadRouteOwners(ctx context.Context, limit int) ([]sessionstore.Route, map[string]sessionstore.Lease, error) {
	records, err := s.LoadRoutes(ctx, limit)
	owners := make(map[string]sessionstore.Lease)
	for _, record := range records {
		lease, readErr := s.Get(ctx, record.SessionID)
		if readErr != nil {
			err = errors.Join(err, readErr)
		} else {
			owners[record.SessionID] = lease
		}
	}
	return records, owners, err
}

func TestRestoreNewestLeaseFailureNeverFallsBackBothStores(t *testing.T) {
	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("batched=%v", batched), func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprint(reverse), func(t *testing.T) {
					routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
						sys := startTestRelay(t, Config{Owners: store})
						a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
						caller := newTestCaller(t)
						leaseA, err := store.Get(context.Background(), sessionA)
						require.NoError(t, err)
						leaseB, err := store.Get(context.Background(), sessionB)
						require.NoError(t, err)
						now := time.Now().UTC()
						old := sessionstore.Route{Caller: caller.addr(), SessionID: sessionA, Generation: leaseA.Epoch, ConfirmedAt: now, LastAuthenticated: now, ExpiresAt: now.Add(time.Minute)}
						newest := old
						newest.SessionID, newest.Generation = sessionB, leaseB.Epoch
						newest.LastAuthenticated = now.Add(time.Millisecond)
						records := []sessionstore.Route{old, newest}
						if reverse {
							records[0], records[1] = records[1], records[0]
						}
						for _, record := range records {
							require.NoError(t, routes.PutRoute(context.Background(), record, false))
						}
						failed := &failedLeaseRouteStore{partialRouteStore: partialRouteStore{Routes: routes, records: records}, failedSession: sessionB}
						sys.cfg.Routes = failed
						if batched {
							sys.cfg.Routes = &failedOwnedRouteStore{failedLeaseRouteStore: failed}
						}
						sys.restart(t)
						_, restored := sys.relay.flows.forwardRoute(caller.addr())
						require.False(t, restored, "an unreadable newer lease must reserve the address without restoring it")
						require.EqualValues(t, 0, sys.relay.Stats().RoutesRestored)
						require.EqualValues(t, 2, sys.relay.Stats().RoutesRestoreSkipped)
						require.EqualValues(t, 1, sys.relay.Stats().RoutesRestoreFailed)
						caller.send(t, media, sys.relay.PublicAddr())
						a.expectNothing(t)
						b.expectNothing(t)
						loaded, err := routes.LoadRoutes(context.Background(), 10)
						require.NoError(t, err)
						require.Equal(t, []sessionstore.Route{newest}, loaded, "keep the unreadable winner for a later restart")
					})
				})
			}
		})
	}
}

func TestNominationSwitchToConfirmedBackupPreservesBothStores(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		worker := sys.worker(t, sessionA)
		primary, backup := newTestCaller(t), newTestCaller(t)
		nominate := func(caller *testCaller) {
			request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(sessionA+":callerufrag"), stun.RawAttribute{Type: stun.AttrUseCandidate})
			require.NoError(t, err)
			caller.send(t, request.Raw, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), request.Raw)
			sys.confirm(t, worker, caller, request.Raw)
		}
		// Primary is nominated first; backup is confirmed without nomination.
		nominate(primary)
		original := persistedRoute(t, routes, primary.addr(), func(sessionstore.Route) bool { return true })
		sys.connect(t, backup, worker, sessionA)
		persistedRoute(t, routes, backup.addr(), func(sessionstore.Route) bool { return true })
		// Force the renewal through persistence, rather than relying on throttling.
		time.Sleep(time.Second + 10*time.Millisecond)
		nominate(backup)
		persistedRoute(t, routes, backup.addr(), func(record sessionstore.Route) bool {
			return record.LastAuthenticated.After(original.LastAuthenticated.Add(time.Second))
		})
		loaded, err := routes.LoadRoutes(context.Background(), 10)
		require.NoError(t, err)
		require.Len(t, loaded, 2)
		require.Contains(t, loaded, original)
		for _, caller := range []*testCaller{primary, backup} {
			caller.send(t, media, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), media)
		}
		sys.restart(t)
		require.EqualValues(t, 2, sys.relay.Stats().RoutesRestored)
		for _, caller := range []*testCaller{primary, backup} {
			caller.send(t, media, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), media)
		}
	})
}
