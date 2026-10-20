package controlplane

import (
	"context"
	"encoding/json"
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

func TestCreateRecordForWorkerDyingDuringRebuild(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	addrA := netip.MustParseAddrPort("127.0.0.1:23493")
	addrB := netip.MustParseAddrPort("127.0.0.1:23494")
	_, err := store.Claim(ctx, "live-on-a", addrA, time.Minute)
	require.NoError(t, err)
	var aDown atomic.Bool
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if aDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		privateapi.Write(w, mediaworker.WorkerStatus{Address: addrA, InventoryKnown: true})
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: addrB, InventoryKnown: true})
	}))
	defer serverB.Close()
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	started := time.Now()
	lease, err := store.ClaimControl(ctx, "probe", 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: started.Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	data, err := json.Marshal(createIntent{Worker: "a", Started: time.Now()})
	require.NoError(t, err)
	require.NoError(t, store.PutControlRecord(ctx, lease, "create:interrupted", data))
	fixed := []FixedWorker{{Name: "a", URL: serverA.URL}, {Name: "b", URL: serverB.URL}}
	p.SetRelayReady(false)
	first := p.rebuild(ctx, fixed)
	t.Logf("first attempt (relay not ready): %v", first)
	require.ErrorIs(t, first, ErrNotReady)
	aDown.Store(true)
	p.SetRelayReady(true)
	var last error
	for range 5 {
		last = p.rebuild(ctx, fixed)
	}
	p.mu.Lock()
	dead := p.workers["a"].dead
	p.mu.Unlock()
	t.Logf("later attempts: %v; worker a dead=%t", last, dead)
	require.NoError(t, last, "rebuild never completes, so detection never starts")
	require.False(t, dead, "only heartbeat detection may establish death")
	records, err := store.ControlRecords(ctx, lease)
	require.NoError(t, err)
	require.Contains(t, records, "create:interrupted", "failed observation must retain its journal")
}

// Whichever record is visited first fails observation. The second must still
// settle on this tick, independently of Go's map iteration order.
func TestCreateReconciliationContinuesAfterStatusFailure(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	p := New(&fakeRelay{}, store)
	var failFirst atomic.Bool
	failFirst.Store(true)
	for i, name := range []string{"a", "b"} {
		address := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(23511+i))
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if failFirst.CompareAndSwap(true, false) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			privateapi.Write(w, mediaworker.WorkerStatus{Address: address, InventoryKnown: true})
		}))
		t.Cleanup(worker.Close)
		require.NoError(t, p.Register(name, address, &RemoteWorker{URL: worker.URL}))
	}
	authorizeTestControl(t, p)
	for _, worker := range p.workers {
		worker.reserved++
		key, err := p.startCreate(ctx, worker)
		require.NoError(t, err)
		require.ErrorIs(t, p.finishCreate(ctx, key, worker, "", privateapi.Uncertain(context.DeadlineExceeded)), privateapi.ErrUncertain)
	}
	require.NoError(t, p.reconcileCreates(ctx))
	records, err := store.ControlRecords(ctx, p.restart.guard.Load().lease)
	require.NoError(t, err)
	require.Len(t, records, 1, "only the failed observation must remain unresolved")
	require.Len(t, p.restart.creates, 1)
	reserved := 0
	for _, worker := range p.workers {
		require.False(t, worker.dead)
		reserved += worker.reserved
	}
	require.Equal(t, 1, reserved)
	require.NoError(t, p.reconcileCreates(ctx))
	require.Empty(t, p.restart.creates)
	for _, worker := range p.workers {
		require.Zero(t, worker.reserved)
	}
}
