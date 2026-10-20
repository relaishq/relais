package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type observedRebuildIntents struct {
	*sessionstore.Memory
	armed atomic.Bool
	read  chan struct{}
}

func (s *observedRebuildIntents) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	intents, err := s.Memory.ControlIntents(ctx)
	if s.armed.CompareAndSwap(true, false) {
		close(s.read)
	}
	return intents, err
}

type workerTransportFunc func(*http.Request) (*http.Response, error)

func (f workerTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// An End handler holds c.mu through the worker request. Its transport observes
// that handler's intent while the successor rebuild reaches the same call.
func TestNewEpochRebuildWaitsForInFlightEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := &observedRebuildIntents{Memory: sessionstore.NewMemory(), read: make(chan struct{})}
	address := netip.MustParseAddrPort("127.0.0.1:23502")
	lease, err := store.Claim(ctx, "ending", address, time.Minute)
	require.NoError(t, err)
	p := New(&fakeRelay{}, store)
	c := p.rebuildCall(lease)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var observed atomic.Int64
	transport := workerTransportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			close(entered)
			for {
				select {
				case <-release:
					return nil, errors.New("end response unavailable")
				default:
					// This executes on the real End handler's goroutine, under c.mu.
					// Removing rebuild's lock makes these reads race its intent writes.
					intent := c.intent
					if intent != nil {
						observed.Store(int64(len(intent.Token)))
					}
					runtime.Gosched()
				}
			}
		}
		response := httptest.NewRecorder()
		privateapi.Write(response, mediaworker.WorkerStatus{Address: address, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: lease.SessionID, Owner: address, Epoch: lease.Epoch}}})
		return response.Result(), nil
	})
	require.NoError(t, p.Register("a", address, &RemoteWorker{URL: "http://worker.invalid", Client: &http.Client{Transport: transport}}))
	authorizeTestControl(t, p)
	p.SetRelayReady(true)
	handlerDone := make(chan struct{})
	response := httptest.NewRecorder()
	go func() {
		defer close(handlerDone)
		p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/calls/ending", nil))
	}()
	defer func() { releaseOnce.Do(func() { close(release) }); <-handlerDone }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("End handler did not reach the worker")
	}
	require.Eventually(t, func() bool { return observed.Load() > 0 }, time.Second, time.Millisecond)
	old := p.restart.guard.Load().lease
	require.NoError(t, store.ReleaseControl(ctx, old))
	started := time.Now()
	next, err := store.ClaimControl(ctx, "successor", ControlLeaseTTL)
	require.NoError(t, err)
	require.Greater(t, next.Epoch, old.Epoch)
	p.restart.guard.Store(&controlGuard{lease: next, deadline: started.Add(ControlLeaseTTL)})
	p.restart.ready.Store(false)
	store.armed.Store(true)
	rebuilt, rebuildDone := make(chan error, 1), make(chan struct{})
	go func() { defer close(rebuildDone); rebuilt <- p.rebuild(ctx, nil) }()
	defer func() { releaseOnce.Do(func() { close(release) }); <-rebuildDone }()
	select {
	case <-store.read:
	case <-ctx.Done():
		t.Fatal("rebuild did not reach the stored intent")
	}
	require.Never(t, func() bool {
		select {
		case err := <-rebuilt:
			rebuilt <- err
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond, "rebuild bypassed the in-flight handler's call lock")
	releaseOnce.Do(func() { close(release) })
	<-handlerDone
	require.GreaterOrEqual(t, response.Code, 400)
	require.NoError(t, <-rebuilt)
	_, err = store.Get(ctx, lease.SessionID)
	require.ErrorIs(t, err, sessionstore.ErrEnded)
}
