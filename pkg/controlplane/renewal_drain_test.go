package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Renewal can commit even when its response is lost. The local deadline then
// expires while the store still retains the same tenure.
type lostControlRenewalReply struct {
	*sessionstore.Memory
	lose         atomic.Bool
	rebuildReads atomic.Int64
}

func (s *lostControlRenewalReply) RenewControl(ctx context.Context, lease sessionstore.ControlLease, ttl time.Duration) (sessionstore.ControlLease, error) {
	next, err := s.Memory.RenewControl(ctx, lease, ttl)
	if err == nil && s.lose.Load() {
		return sessionstore.ControlLease{}, sessionstore.ErrTransient
	}
	return next, err
}
func (s *lostControlRenewalReply) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	s.rebuildReads.Add(1)
	return s.Memory.ControlIntents(ctx)
}

func TestSameEpochRenewalPreservesInFlightDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for local control lease deadline")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &lostControlRenewalReply{Memory: sessionstore.NewMemory()}
	a := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:23491"), running: map[string]bool{}, exported: make(chan struct{}, 1), exportGate: make(chan struct{})}
	b := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:23492"), running: map[string]bool{}}
	var release sync.Once
	defer release.Do(func() { close(a.exportGate) })
	id, _, err := a.CreateSession(ctx, "offer")
	require.NoError(t, err)
	workerServer := func(worker *fakeWorker) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/status":
				worker.mu.Lock()
				ids := make([]string, 0, len(worker.running))
				for id := range worker.running {
					ids = append(ids, id)
				}
				worker.mu.Unlock()
				status := mediaworker.WorkerStatus{Address: worker.addr, InventoryKnown: true}
				for _, id := range ids {
					lease, err := store.Get(r.Context(), id)
					if err == nil {
						status.Inventory = append(status.Inventory, mediaworker.InstalledSession{ID: id, Owner: lease.Worker, Epoch: lease.Epoch})
					}
				}
				privateapi.Write(w, status)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/export"):
				state, err := worker.ExportSession(id)
				if err != nil {
					privateapi.Error(w, err, mediaworker.RemoteErrors)
					return
				}
				privateapi.Write(w, mediaworker.ExportReply{State: state})
			case r.Method == http.MethodPost && r.URL.Path == "/resume":
				var request mediaworker.ResumeRequest
				if !privateapi.Read(w, r, &request) {
					return
				}
				resumed, err := worker.ResumeSession(request.State, mediaworker.ResumeOptions{Lease: request.Lease, SequenceMargin: request.SequenceMargin})
				if err != nil {
					privateapi.Error(w, err, mediaworker.RemoteErrors)
					return
				}
				privateapi.Write(w, mediaworker.ResumeReply{ID: resumed})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		return server
	}
	serverA, serverB := workerServer(a), workerServer(b)
	p := NewWithConfig(&fakeRelay{}, store, Config{DeadAfter: time.Hour})
	require.NoError(t, p.EnableRestart())
	p.SetRelayReady(true)
	done := make(chan error, 1)
	go func() {
		done <- p.RunManaged(ctx, []FixedWorker{{Name: "a", URL: serverA.URL}, {Name: "b", URL: serverB.URL}})
	}()
	defer func() {
		release.Do(func() { close(a.exportGate) })
		cancel()
		require.NoError(t, <-done)
		require.NoError(t, p.ReleaseControl(context.Background()))
	}()
	require.Eventually(t, p.mutationReady, 3*time.Second, 10*time.Millisecond)
	p.mu.Lock()
	p.workers["a"].worker.(*RemoteWorker).Client = &http.Client{Timeout: 15 * time.Second}
	p.mu.Unlock()
	initialReads := store.rebuildReads.Load()
	epoch := p.restart.guard.Load().lease.Epoch
	drainDone := make(chan error, 1)
	go func() {
		moves, err := p.Drain(ctx, "a")
		if err == nil && len(moves) != 1 {
			t.Errorf("drain moves=%v", moves)
		}
		drainDone <- err
	}()
	select {
	case <-a.exported:
	case <-time.After(3 * time.Second):
		t.Fatal("drain never reached export")
	}
	before, err := store.Memory.ControlIntents(ctx)
	require.NoError(t, err)
	require.Len(t, before, 1)
	store.lose.Store(true)
	require.Eventually(t, func() bool {
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		return !p.controlAuthority() && !running
	}, 3*time.Second, 10*time.Millisecond)
	require.False(t, p.mutationReady())
	store.lose.Store(false)
	require.Eventually(t, p.mutationReady, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, epoch, p.restart.guard.Load().lease.Epoch)
	require.Equal(t, initialReads, store.rebuildReads.Load(), "same tenure must not rebuild this process's active intents")
	after, err := store.Memory.ControlIntents(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "renewal changed the in-flight planned move")
	release.Do(func() { close(a.exportGate) })
	select {
	case err := <-drainDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
	require.True(t, b.runs(id))
	require.False(t, a.runs(id))
	current, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, b.addr, current.Worker)
	require.Zero(t, p.lostCount)
	intents, err := store.Memory.ControlIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, intents)
	require.Empty(t, p.RebuildStatus().Outcomes, "live work was reported as interrupted")
}
