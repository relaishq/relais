package callharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type recordingStatusContext struct{}

type recordingStatusStore struct {
	sessionstore.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *recordingStatusStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if ctx.Value(recordingStatusContext{}) == true && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return s.Store.Get(ctx, id)
}

// The real periodic collector reaches the real /status handler, which holds
// the control-plane call lock during Get. A caller move must wait for this
// observational read instead of failing as if another move owned that lock.
func TestRecordingStatusPollPreservesPlannedMoves(t *testing.T) {
	for _, operation := range []string{"handover", "drain"} {
		t.Run(operation, func(t *testing.T) {
			forWrapSessionStores(t, func(t *testing.T, store sessionstore.Store) {
				if store == nil {
					store = sessionstore.NewMemory()
				}
				gated := &recordingStatusStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
				h, err := Start(Options{Relay: true, Workers: 2, SessionStore: gated})
				require.NoError(t, err)
				var server *httptest.Server
				var once sync.Once
				release := func() { once.Do(func() { close(gated.release) }) }
				t.Cleanup(func() {
					release()
					require.NoError(t, h.Close())
					if server != nil {
						server.Close()
					}
				})
				// Tag only status requests, not worker reads or move reconciliation.
				// All requests still execute the production control-plane handler.
				handler := h.workers.relay.plane.Handler()
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/status" {
						r = r.WithContext(context.WithValue(r.Context(), recordingStatusContext{}, true))
					}
					handler.ServeHTTP(w, r)
				}))
				h.signalingURL = server.URL + "/calls"
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				gated.armed.Store(true)
				select {
				case <-gated.entered:
				case <-ctx.Done():
					t.Fatal("periodic recording collector never reached status Get")
				}
				result := make(chan error, 1)
				go func() {
					if operation == "handover" {
						result <- call.Handover(HandoverOptions{To: 1})
					} else {
						result <- h.Drain(0)
					}
				}()
				select {
				case err := <-result:
					require.FailNow(t, "planned move overlapped the recording status read", "result: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				release()
				select {
				case err := <-result:
					require.NoError(t, err)
				case <-ctx.Done():
					t.Fatal("planned move did not complete after status read finished")
				}
			})
		})
	}
}

type recordingTransferStore struct {
	sessionstore.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *recordingTransferStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return s.Store.Transfer(ctx, from, to, ttl)
}

// Hold a real planned transfer past a maintenance tick. Compaction must keep
// running, and another actual move on that call must still fail fast.
func TestRecordingMaintenanceDuringPlannedMove(t *testing.T) {
	forWrapSessionStores(t, func(t *testing.T, store sessionstore.Store) {
		if store == nil {
			store = sessionstore.NewMemory()
		}
		gated := &recordingTransferStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
		h, err := Start(Options{Relay: true, Workers: 2, SessionStore: gated})
		require.NoError(t, err)
		var once sync.Once
		release := func() { once.Do(func() { close(gated.release) }) }
		t.Cleanup(func() { release(); require.NoError(t, h.Close()) })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := h.Dial(ctx, CallOptions{Recording: RecordingOptions{History: 10 * time.Second}})
		require.NoError(t, err)
		other, err := h.Dial(ctx, CallOptions{Worker: 0})
		require.NoError(t, err)
		gated.armed.Store(true)
		first := make(chan error, 1)
		go func() { first <- call.Handover(HandoverOptions{To: 1}) }()
		select {
		case <-gated.entered:
		case <-ctx.Done():
			t.Fatal("planned move never reached Transfer")
		}
		call.rec.mu.Lock()
		call.rec.start = call.rec.start.Add(-20 * time.Second)
		call.rec.sentAudioUnits["old history fixture"] = contentUnit{at: time.Second, written: true}
		call.rec.mu.Unlock()
		second := make(chan error, 1)
		go func() { second <- call.Handover(HandoverOptions{To: 1}) }()
		select {
		case err := <-second:
			require.ErrorIs(t, err, controlplane.ErrMoveInProgress)
		case <-time.After(time.Second):
			t.Fatal("an actual overlapping move was serialized instead of rejected")
		}

		// A different call can still move while the first transfer is held.
		parallel := make(chan error, 1)
		go func() { parallel <- other.Handover(HandoverOptions{To: 1}) }()
		select {
		case err := <-parallel:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("an unrelated planned move was serialized behind the first")
		}
		require.Eventually(t, func() bool {
			gauge := call.Recording()
			return gauge.HistoryStart > 0 && gauge.ContentUnits == 0
		}, 3*time.Second, 10*time.Millisecond, "recording must compact while the move is blocked")
		release()
		select {
		case err := <-first:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("planned move never completed after Transfer was released")
		}
	})
}
