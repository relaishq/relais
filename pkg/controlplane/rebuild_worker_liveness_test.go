package controlplane

import (
	"context"
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

func TestRebuildSlowStatusWorkerKeepsHeartbeats(t *testing.T) {
	for _, delay := range []time.Duration{300 * time.Millisecond, 900 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			store := sessionstore.NewMemory()
			source := netip.MustParseAddrPort("127.0.0.1:23521")
			target := netip.MustParseAddrPort("127.0.0.1:23522")
			live, err := store.Claim(context.Background(), "slow-live-status", source, time.Minute)
			require.NoError(t, err)
			var slowReplies atomic.Int32
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !restartWait(r.Context(), delay) {
					return
				}
				slowReplies.Add(1)
				privateapi.Write(w, mediaworker.WorkerStatus{Address: source, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: live.SessionID, Owner: source, Epoch: live.Epoch}}})
			}))
			defer slow.Close()
			fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				privateapi.Write(w, mediaworker.WorkerStatus{Address: target, InventoryKnown: true})
			}))
			defer fast.Close()
			p := New(&fakeRelay{}, store)
			require.NoError(t, p.EnableRestart())
			p.SetRelayReady(true)
			// Restore the same worker identities, as a successor does.
			lease, err := store.ClaimControl(context.Background(), "predecessor", ControlLeaseTTL)
			require.NoError(t, err)
			p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(ControlLeaseTTL)})
			fixed := []FixedWorker{{Name: "slow", URL: slow.URL, Address: source}, {Name: "fast", URL: fast.URL, Address: target}}
			for _, worker := range fixed {
				require.NoError(t, p.controlRecord(context.Background(), "worker:"+worker.Name, worker))
			}
			require.NoError(t, store.ReleaseControl(context.Background(), lease))
			p.restart.guard.Store(nil)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- p.RunManaged(ctx, fixed) }()
			var heartbeatFailures atomic.Int32
			heartbeatDone := make(chan struct{})
			go func() {
				defer close(heartbeatDone)
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						for _, worker := range fixed {
							p.mu.Lock()
							exists := p.workers[worker.Name] != nil
							p.mu.Unlock()
							if exists {
								if _, err := p.remoteHeartbeat(worker.Name, HeartbeatRequest{Address: worker.Address}); err != nil {
									heartbeatFailures.Add(1)
								}
							}
						}
					}
				}
			}()
			defer func() {
				cancel()
				<-heartbeatDone
				require.NoError(t, <-done)
				require.NoError(t, p.ReleaseControl(context.Background()))
			}()
			require.Eventually(t, p.mutationReady, 3*time.Second, 10*time.Millisecond)
			p.mu.Lock()
			dead := p.workers["slow"].observation.knowledge().dead
			p.mu.Unlock()
			t.Logf("status_delay=%s registered_dead=%t heartbeat_failures=%d", delay, dead, heartbeatFailures.Load())
			require.False(t, dead, "a slow status response must not declare a heartbeating worker dead")
			// Longer than two detection windows, including a successful 900 ms status
			// response on the background observer's one-second request budget.
			require.Never(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.workers["slow"].observation.knowledge().dead }, 1300*time.Millisecond, 10*time.Millisecond)
			require.Zero(t, heartbeatFailures.Load())
			require.Positive(t, slowReplies.Load())
			current, err := store.Get(context.Background(), live.SessionID)
			require.NoError(t, err)
			require.Equal(t, live.Epoch, current.Epoch, "live calls must retain the original owner tenure")
			require.Equal(t, source, current.Worker)
			p.mu.Lock()
			lost := p.lostCount
			p.mu.Unlock()
			require.Zero(t, lost)
		})
	}
}

func TestRebuildUnknownWorkerWaitsForHeartbeatBeforePlacement(t *testing.T) {
	store := sessionstore.NewMemory()
	source := netip.MustParseAddrPort("127.0.0.1:23523")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer slow.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	p.SetRelayReady(true)
	require.NoError(t, p.controlRecord(context.Background(), "worker:a", FixedWorker{Name: "a", URL: slow.URL, Address: source}))
	fixed := []FixedWorker{{Name: "a", URL: slow.URL}}
	require.NoError(t, p.rebuild(context.Background(), fixed))
	require.NoError(t, p.rebuild(context.Background(), fixed))
	_, err := p.pick(context.Background(), "a", netip.AddrPort{})
	require.ErrorIs(t, err, ErrNoTarget, "unknown liveness must not become free capacity")
	require.NoError(t, p.SamplePool(context.Background()))
	require.Equal(t, 1, p.PoolSnapshot().Pending)
	require.Zero(t, p.PoolSnapshot().Ready)
	status, err := p.Status(context.Background())
	require.NoError(t, err)
	require.True(t, status.Workers[0].LivenessUnknown)
	_, err = p.remoteHeartbeat("a", HeartbeatRequest{Address: source, Token: "prior-completed-token"})
	require.NoError(t, err, "a slow status response must not reject the worker's prior heartbeat token")
	worker, err := p.pick(context.Background(), "a", netip.AddrPort{})
	require.NoError(t, err)
	p.unreserve(worker)
	require.NoError(t, p.SamplePool(context.Background()))
	require.Zero(t, p.PoolSnapshot().Pending)
	require.Equal(t, 1, p.PoolSnapshot().Ready)
}

type heartbeatDuringRebuildStore struct {
	*sessionstore.Memory
	heartbeat func()
	once      atomic.Bool
}

func (s *heartbeatDuringRebuildStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if s.once.CompareAndSwap(false, true) {
		s.heartbeat()
	}
	return s.Memory.ListByWorker(ctx, addr)
}

func TestRebuildHeartbeatDoesNotRepeatFailedCreateObservation(t *testing.T) {
	store := &heartbeatDuringRebuildStore{Memory: sessionstore.NewMemory()}
	addr := netip.MustParseAddrPort("127.0.0.1:23524")
	var reads atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { reads.Add(1); <-r.Context().Done() }))
	defer worker.Close()
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	lease, err := store.ClaimControl(context.Background(), "rebuild", 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	p.SetRelayReady(true)
	require.NoError(t, p.controlRecord(context.Background(), "worker:a", FixedWorker{Name: "a", URL: worker.URL, Address: addr}))
	require.NoError(t, p.controlRecord(context.Background(), "create:interrupted", createIntent{Worker: "a"}))
	store.heartbeat = func() { _, err := p.remoteHeartbeat("a", HeartbeatRequest{Address: addr}); require.NoError(t, err) }
	started := time.Now()
	require.NoError(t, p.rebuild(context.Background(), []FixedWorker{{Name: "a", URL: worker.URL}}))
	elapsed := time.Since(started)
	t.Logf("heartbeat_during_rebuild=%s status_reads=%d", elapsed, reads.Load())
	require.Less(t, elapsed, 500*time.Millisecond, "heartbeat acceptance must not cause another status timeout for a saved create")
	require.EqualValues(t, 1, reads.Load())
	records, err := store.ControlRecords(context.Background(), lease)
	require.NoError(t, err)
	require.Contains(t, records, "create:interrupted", "uncertain create must retain its journal")
}
