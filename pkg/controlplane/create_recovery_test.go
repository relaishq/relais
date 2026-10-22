package controlplane

import (
	"context"
	"encoding/json"
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

// Regression: a persisted create record that names a worker which is down at
// restart and still has leases makes every rebuild attempt fail.
func TestCreateRecordForDownWorkerRebuilds(t *testing.T) {
	for _, withCreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-create-record", true: "create-record"}[withCreate], func(t *testing.T) {
			ctx := context.Background()
			store := sessionstore.NewMemory()
			addrA := netip.MustParseAddrPort("127.0.0.1:23461")
			addrB := netip.MustParseAddrPort("127.0.0.1:23462")
			_, err := store.Claim(ctx, "live-on-a", addrA, time.Minute)
			require.NoError(t, err)
			down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer down.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				privateapi.Write(w, mediaworker.WorkerStatus{Address: addrB, InventoryKnown: true})
			}))
			defer up.Close()
			p := New(&fakeRelay{}, store)
			require.NoError(t, p.EnableRestart())
			started := time.Now()
			lease, err := store.ClaimControl(ctx, "test-control", ControlLeaseTTL)
			require.NoError(t, err)
			p.restart.guard.Store(&controlGuard{lease: lease, deadline: started.Add(time.Hour)})
			put := func(key string, value any) {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				require.NoError(t, store.PutControlRecord(ctx, lease, key, data))
			}
			put("worker:a", FixedWorker{Name: "a", URL: down.URL, Address: addrA})
			put("worker:b", FixedWorker{Name: "b", URL: up.URL, Address: addrB})
			if withCreate {
				put("create:interrupted", createIntent{Worker: "a", Started: time.Now()})
			}
			p.SetRelayReady(true)
			fixed := []FixedWorker{{Name: "a", URL: down.URL}, {Name: "b", URL: up.URL}}
			var lastErr error
			for range 5 {
				lastErr = p.rebuild(ctx, fixed)
				if lastErr == nil {
					break
				}
			}
			t.Logf("withCreate=%t last rebuild error=%v", withCreate, lastErr)
			require.NoError(t, lastErr)
		})
	}
}

type failingEndWorker struct {
	*fakeWorker
	fail atomic.Bool
}

func (w *failingEndWorker) EndSession(id string) error {
	if w.fail.Load() {
		return privateapi.Uncertain(errors.New("worker unreachable"))
	}
	return w.fakeWorker.EndSession(id)
}

// Regression: one failed End leaves an "end" intent that refuses every later move
// and takeover of that live call in a control that never restarts.
func TestFailedEndAllowsLaterMoveAndTakeover(t *testing.T) {
	for _, operation := range []string{"move", "drain", "takeover"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			m := sessionstore.NewMemory()
			p := New(&fakeRelay{}, m)
			a := &failingEndWorker{fakeWorker: &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:1"), running: map[string]bool{}}}
			b := &fakeWorker{store: m, addr: netip.MustParseAddrPort("127.0.0.1:2"), running: map[string]bool{}}
			require.NoError(t, p.Register("a", a.addr, a))
			require.NoError(t, p.Register("b", b.addr, b))
			authorizeTestControl(t, p)
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			a.fail.Store(true)
			require.Error(t, p.End(ctx, id))
			a.fail.Store(false)
			switch operation {
			case "move":
				_, err = p.Move(ctx, id, "b")
				require.NoError(t, err)
			case "drain":
				_, err = p.Drain(ctx, "a")
				require.NoError(t, err)
			default:
				lease, err := m.Get(ctx, id)
				require.NoError(t, err)
				require.NoError(t, m.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
				p.mu.Lock()
				source := p.workers["a"]
				expireObservedWorker(t, source)
				p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
				p.mu.Unlock()
				p.takeover(ctx, source, lease, time.Now())
				current, err := m.Get(ctx, id)
				require.NoError(t, err)
				require.Equal(t, b.addr, current.Worker)
				require.True(t, b.runs(id))
				require.Zero(t, p.lostCount)
			}
		})
	}
}

// A reconciliation tick must not queue a global writer behind a slow create.
func TestSlowCreateDoesNotBlockHealthyWorker(t *testing.T) {
	ctx := context.Background()
	p, a, _, _ := setup(t)
	require.NoError(t, p.EnableRestart())
	lease, err := p.restart.store.ClaimControl(ctx, "test-control", 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	p.restart.ready.Store(true)
	a.createStarted = make(chan struct{})
	a.createGate = make(chan struct{})
	slowDone := make(chan error, 1)
	go func() { _, _, err := p.Create(ctx, "offer", "a"); slowDone <- err }()
	<-a.createStarted
	reconciled := make(chan struct{})
	go func() { _ = p.reconcileCreates(ctx); close(reconciled) }() // one RunManaged tick
	time.Sleep(50 * time.Millisecond)
	fastDone := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		_, _, err := p.Create(ctx, "offer", "b")
		if err != nil {
			t.Errorf("create on b: %v", err)
		}
		fastDone <- time.Since(started)
	}()
	select {
	case elapsed := <-fastDone:
		t.Logf("create on healthy worker b completed in %s while a is slow", elapsed)
	case <-time.After(2 * time.Second):
		t.Error("healthy worker create blocked behind a slow create")
		close(a.createGate)
		elapsed := <-fastDone
		t.Logf("create on b completed %s after start, only once a's create finished", elapsed)
		require.NoError(t, <-slowDone)
		<-reconciled
		return
	}
	close(a.createGate)
	require.NoError(t, <-slowDone)
	<-reconciled

}
