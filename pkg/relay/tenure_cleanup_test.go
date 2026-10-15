package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// The actual private HTTP cleanup must preserve an epoch transferred away and
// back to the same address, its snapshot, persisted route, and media routing.
func TestTenureCleanupPreservesRacingTransfer(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		ctx := context.Background()
		old, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		next, err := store.Transfer(ctx, old, b.addr(), time.Minute)
		require.NoError(t, err)
		next, err = store.Transfer(ctx, next, a.addr(), time.Minute)
		require.NoError(t, err)
		require.NoError(t, store.PutState(ctx, next, []byte("successor snapshot")))
		require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), a.addr(), next.Epoch))
		record := persistedRoute(t, routes, caller.addr(), func(r sessionstore.Route) bool { return r.Generation == next.Epoch })
		require.NoError(t, store.Release(ctx, old))
		data, err := json.Marshal(old)
		require.NoError(t, err)
		response := httptest.NewRecorder()
		sys.relay.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/sessions/"+sessionA+"/forget-tenure", bytes.NewReader(data)))
		require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
		got, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		require.Equal(t, next, got)
		state, err := store.GetState(ctx, sessionA)
		require.NoError(t, err)
		require.Equal(t, "successor snapshot", string(state))
		require.Equal(t, record, persistedRoute(t, routes, caller.addr(), func(r sessionstore.Route) bool { return r.Generation == next.Epoch }))
		caller.send(t, media, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), media)
		a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	})
}

func TestTenureCleanupRemovesExpiredRoute(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		a := sys.worker(t, sessionA)
		caller := newTestCaller(t)
		sys.connect(t, caller, a, sessionA)
		ctx := context.Background()
		old, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		_, err = store.Renew(ctx, old, time.Millisecond)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		data, err := json.Marshal(old)
		require.NoError(t, err)
		response := httptest.NewRecorder()
		sys.relay.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/sessions/"+sessionA+"/forget-tenure", bytes.NewReader(data)))
		require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
		require.Zero(t, sys.relay.Stats().Flows)
		_, err = store.GetState(ctx, sessionA)
		require.ErrorIs(t, err, sessionstore.ErrNotFound)
		restored, err := routes.LoadRoutes(ctx, 100)
		require.NoError(t, err)
		require.Empty(t, restored)
	})
}

// Even if a successor later expires, an older cleanup token must preserve
// that successor's cached route. This epoch is learned by ordinary lookup,
// without a trusted MoveSessionAtEpoch notification.
func TestTenureCleanupPreservesNewerLookupEpoch(t *testing.T) {
	routeStores(t, func(t *testing.T, store sessionstore.Store, routes sessionstore.Routes) {
		sys := startTestRelay(t, Config{Owners: store, Routes: routes})
		worker := sys.worker(t, sessionA)
		other := sys.worker(t, sessionB)
		caller := newTestCaller(t)
		sys.connect(t, caller, worker, sessionA)
		ctx := context.Background()
		old, err := store.Get(ctx, sessionA)
		require.NoError(t, err)
		next, err := store.Transfer(ctx, old, other.addr(), time.Minute)
		require.NoError(t, err)
		next, err = store.Transfer(ctx, next, worker.addr(), time.Minute)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			check := bindingRequest(t, sessionA)
			caller.send(t, check, sys.relay.PublicAddr())
			worker.expect(t, caller.addr(), check)
			sys.confirm(t, worker, caller, check)
			records, err := routes.LoadRoutes(ctx, 100)
			if err != nil {
				return false
			}
			for _, record := range records {
				if record.Caller == caller.addr() && record.Generation == next.Epoch {
					return true
				}
			}
			return false
		}, 2*time.Second, 20*time.Millisecond)
		_, err = store.Renew(ctx, next, time.Millisecond)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		invoke := func(lease sessionstore.Lease) {
			data, err := json.Marshal(lease)
			require.NoError(t, err)
			response := httptest.NewRecorder()
			sys.relay.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/sessions/"+sessionA+"/forget-tenure", bytes.NewReader(data)))
			require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
		}
		invoke(old)
		require.Equal(t, 1, sys.relay.Stats().Flows)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
		invoke(next)
		require.Zero(t, sys.relay.Stats().Flows)
	})
}
