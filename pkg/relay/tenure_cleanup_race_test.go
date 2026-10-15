package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type cleanupReadStore struct {
	sessionstore.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *cleanupReadStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	lease, err := s.Store.Get(ctx, id)
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return lease, err
}

func TestTenureCleanupRejectsReadRacingRouteGeneration(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, _ sessionstore.Routes) {
		owners := &cleanupReadStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
		sys := startTestRelay(t, Config{Owners: owners})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		old, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		owners.armed.Store(true)
		done := make(chan error, 1)
		go func() { done <- sys.relay.ForgetSessionTenure(ctx, old) }()
		select {
		case <-owners.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		next, err := store.Transfer(ctx, old, b.addr(), time.Minute)
		require.NoError(t, err)
		require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
		next, err = store.Transfer(ctx, next, a.addr(), time.Minute)
		require.NoError(t, err)
		require.NoError(t, store.PutState(ctx, next, []byte("successor")))
		// Legacy trusted moves carry epoch zero. Address equality alone cannot
		// detect this ABA; the scoped generation must reject the stale read.
		require.NoError(t, sys.relay.MoveSession(sessionA, b.addr(), a.addr()))
		close(owners.release)
		require.ErrorContains(t, <-done, "raced with route change")
		caller.send(t, media, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), media)
		current, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		require.Equal(t, next, current)
		state, err := store.GetState(ctx, sessionA)
		require.NoError(t, err)
		require.Equal(t, "successor", string(state))
		require.NoError(t, sys.relay.ForgetSessionTenure(ctx, old))
		caller.send(t, media, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), media)
	})
}
