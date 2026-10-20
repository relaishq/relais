package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestCreateReconciliationStatusFailureKeepsWorkerLive(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	addrA := netip.MustParseAddrPort("127.0.0.1:23481")
	addrB := netip.MustParseAddrPort("127.0.0.1:23482")
	live, err := store.Claim(ctx, "live-on-a", addrA, time.Minute)
	require.NoError(t, err)
	var statusFails atomic.Bool
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if statusFails.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		privateapi.Write(w, mediaworker.WorkerStatus{Address: addrA, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: live.SessionID, Owner: addrA, Epoch: live.Epoch}}})
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: addrB, InventoryKnown: true})
	}))
	defer serverB.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	guard := *p.restart.guard.Load()
	guard.deadline = time.Now().Add(time.Hour)
	p.restart.guard.Store(&guard)
	require.NoError(t, p.Register("a", addrA, &RemoteWorker{URL: serverA.URL}))
	require.NoError(t, p.Register("b", addrB, &RemoteWorker{URL: serverB.URL}))
	p.rebuildCall(live)
	p.mu.Lock()
	a := p.workers["a"]
	a.reserved++
	p.mu.Unlock()
	key, err := p.startCreate(ctx, a)
	require.NoError(t, err)
	require.ErrorIs(t, p.finishCreate(ctx, key, a, "", privateapi.Uncertain(errors.New("request timed out"))), privateapi.ErrUncertain)
	// Worker a keeps heartbeating; only one status read fails.
	_, err = p.remoteHeartbeat("a", HeartbeatRequest{Address: addrA})
	require.NoError(t, err)
	statusFails.Store(true)
	reconcileErr := p.reconcileCreates(ctx)
	p.mu.Lock()
	dead, last := a.dead, a.lastHeartbeat
	p.mu.Unlock()
	t.Logf("reconcile=%v dead=%t heartbeat_age=%s", reconcileErr, dead, time.Since(last))
	_, hbErr := p.remoteHeartbeat("a", HeartbeatRequest{Address: addrA})
	t.Logf("next heartbeat from the live worker: %v", hbErr)
	require.False(t, dead, "a live, heartbeating worker was marked dead by one failed status read")
	require.Error(t, reconcileErr)
	require.NoError(t, hbErr)
	p.mu.Lock()
	require.Equal(t, 1, a.reserved)
	p.mu.Unlock()
	records, err := store.ControlRecords(ctx, guard.lease)
	require.NoError(t, err)
	require.Contains(t, records, key)
	statusFails.Store(false)
	require.NoError(t, p.reconcileCreates(ctx))
	p.mu.Lock()
	require.Zero(t, a.reserved)
	p.mu.Unlock()
	current, err := store.Get(ctx, live.SessionID)
	require.NoError(t, err)
	require.Equal(t, live.Epoch, current.Epoch)
	require.Equal(t, addrA, current.Worker)
}

// Recovery must not depend on the reconciliation tick releasing this record.
func TestTakeoverIgnoresOnlyUnknownCreateReservations(t *testing.T) {
	ctx := context.Background()
	p, _, baseB, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	authorizeTestControl(t, p)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	source := p.workers["a"]
	source.dead, source.reserved = true, 1
	key, err := p.startCreate(ctx, source)
	require.NoError(t, err)
	require.False(t, p.recoverWorker(ctx, source, time.Now()), "active admission still fences recovery")
	require.ErrorIs(t, p.finishCreate(ctx, key, source, "", privateapi.Uncertain(errors.New("lost reply"))), privateapi.ErrUncertain)
	require.True(t, p.recoverWorker(ctx, source, time.Now()), "unknown admission must not block recovery")
	current, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, baseB.addr, current.Worker)
	require.True(t, baseB.runs(id))
	require.Zero(t, p.lostCount)
	require.Equal(t, 1, source.reserved, "no reconciliation released the reservation")
	records, err := p.restart.store.ControlRecords(ctx, p.restart.guard.Load().lease)
	require.NoError(t, err)
	require.Contains(t, records, key, "the unknown create remains durable")
	require.Equal(t, 1, p.unknownCreateReservations(source))
}
