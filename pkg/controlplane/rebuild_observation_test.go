package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type unsettledControlStore struct {
	*sessionstore.Memory
	settlements int
}

func (s *unsettledControlStore) Settle(context.Context, sessionstore.Lease) (sessionstore.Lease, bool, error) {
	s.settlements++
	return sessionstore.Lease{}, false, sessionstore.ErrTransient
}
func TestControlRebuildSettlesCandidateBeforeObservingOwner(t *testing.T) {
	ctx := context.Background()
	store := &unsettledControlStore{Memory: sessionstore.NewMemory()}
	owner := netip.MustParseAddrPort("127.0.0.1:23461")
	lease, err := store.Claim(ctx, "candidate", owner, time.Minute)
	require.NoError(t, err)
	intent := sessionstore.ControlIntent{ID: lease.SessionID, Token: "token", Kind: "move", Lease: lease, Candidate: &lease, Started: time.Now()}
	require.NoError(t, store.PutControlIntent(ctx, intent))
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	p.SetRelayReady(true)
	require.ErrorIs(t, p.rebuild(ctx, nil), sessionstore.ErrTransient)
	require.Equal(t, 1, store.settlements)
	intents, err := store.ControlIntents(ctx)
	require.NoError(t, err)
	require.Len(t, intents, 1)
}
func TestControlCreateReconciliationPreservesOperationClaims(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	owner := netip.MustParseAddrPort("127.0.0.1:23461")
	lease, err := store.Claim(ctx, "moving", owner, time.Minute)
	require.NoError(t, err)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: owner, InventoryKnown: true})
	}))
	defer worker.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	require.NoError(t, p.Register("a", owner, &RemoteWorker{URL: worker.URL}))
	guard := p.restart.guard.Load()
	data, err := json.Marshal(createIntent{Worker: "a"})
	require.NoError(t, err)
	require.NoError(t, store.PutControlRecord(ctx, guard.lease, "create:interrupted", data))
	require.NoError(t, store.PutControlIntent(ctx, sessionstore.ControlIntent{ID: lease.SessionID, Token: "move", Kind: "move", Lease: lease, Started: time.Now()}))
	require.NoError(t, p.reconcileCreates(ctx))
	current, err := store.Get(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, current.Epoch)
}

type routeLimitControlStore struct {
	*sessionstore.Memory
	limit int
}

func (s *routeLimitControlStore) LoadRouteOwners(_ context.Context, limit int) ([]sessionstore.Route, map[string]sessionstore.Lease, error) {
	s.limit = limit
	return nil, nil, nil
}
func TestControlRebuildUsesRelayRouteLimit(t *testing.T) {
	store := &routeLimitControlStore{Memory: sessionstore.NewMemory()}
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	p.SetRelayReady(true)
	require.NoError(t, p.rebuild(context.Background(), nil))
	require.Equal(t, relay.DefaultMaxFlows, store.limit)
	require.Greater(t, store.limit, sessionstore.MaxControlIntents)
}

func TestControlRebuildRejectsInventoryForAnotherWorker(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	a, b := netip.MustParseAddrPort("127.0.0.1:23461"), netip.MustParseAddrPort("127.0.0.1:23462")
	lease, err := store.Claim(ctx, "foreign-observation", b, time.Minute)
	require.NoError(t, err)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: a, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: lease.SessionID, Owner: b, Epoch: lease.Epoch}}})
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	p.SetRelayReady(true)
	saved, err := json.Marshal(FixedWorker{Name: "b", URL: down.URL, Address: b})
	require.NoError(t, err)
	require.NoError(t, store.PutControlRecord(ctx, p.restart.guard.Load().lease, "worker:b", saved))
	require.NoError(t, store.PutControlIntent(ctx, sessionstore.ControlIntent{ID: lease.SessionID, Token: "move", Kind: "move", Lease: lease, Started: time.Now()}))
	require.NoError(t, p.rebuild(ctx, []FixedWorker{{Name: "a", URL: up.URL}, {Name: "b", URL: down.URL}}))
	for _, outcome := range p.RebuildStatus().Outcomes {
		require.NotEqual(t, "live", outcome.Outcome, "another worker's report invented installation")
	}
	require.Equal(t, uint64(1), p.lostCount)
}
func TestControlReplacementClearsPersistedDrain(t *testing.T) {
	p, _, _, _ := setup(t)
	authorizeTestControl(t, p)
	require.NoError(t, p.MarkDraining("a"))
	p.mu.Lock()
	old := p.workers["a"]
	old.dead = true
	old.recovered = true
	p.mu.Unlock()
	require.NoError(t, p.Replace("a", netip.MustParseAddrPort("127.0.0.1:23469"), &RemoteWorker{URL: "http://127.0.0.1:23470"}))
	records, err := p.restart.store.ControlRecords(context.Background(), p.restart.guard.Load().lease)
	require.NoError(t, err)
	require.NotContains(t, records, "drain:a")
}

type resettingStatusRead struct {
	*sessionstore.Memory
	reads int
}

func (s *resettingStatusRead) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	s.reads++
	if s.reads <= 3 {
		return sessionstore.Lease{}, sessionstore.ErrTransient
	}
	return s.Memory.Get(ctx, id)
}
func TestControlStatusRetriesTransientLeaseRead(t *testing.T) {
	ctx := context.Background()
	store := &resettingStatusRead{Memory: sessionstore.NewMemory()}
	addr := netip.MustParseAddrPort("127.0.0.1:23461")
	lease, err := store.Claim(ctx, "live-after-store-resume", addr, time.Minute)
	require.NoError(t, err)
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.Register("a", addr, &fakeWorker{store: store, addr: addr, running: map[string]bool{}}))
	authorizeTestControl(t, p)
	p.rebuildCall(lease)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, lease.Epoch, status.Calls[0].Epoch)
	require.Equal(t, 4, store.reads)
}

func TestControlRebuildWaitsForRelayWorkerRegistration(t *testing.T) {
	ctx := context.Background()
	store := &unsettledControlStore{Memory: sessionstore.NewMemory()}
	owner := netip.MustParseAddrPort("127.0.0.1:23461")
	lease, err := store.Claim(ctx, "candidate-before-relay", owner, time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.PutControlIntent(ctx, sessionstore.ControlIntent{ID: lease.SessionID, Token: "move", Kind: "move", Lease: lease, Candidate: &lease, Started: time.Now()}))
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: owner, InventoryKnown: true})
	}))
	defer worker.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	p.BeginBootstrap([]string{"a"})
	p.SetRelayReady(true)
	fixed := []FixedWorker{{Name: "a", URL: worker.URL}}
	require.ErrorContains(t, p.rebuild(ctx, fixed), "relay workers not registered")
	require.Zero(t, store.settlements, "recovery must wait until its target is registered")
	var registry RelayWorkerRegistry
	require.NoError(t, registry.Sync(ctx, p, &registryRecorder{instance: "relay"}))
	require.ErrorIs(t, p.rebuild(ctx, fixed), sessionstore.ErrTransient)
	require.Equal(t, 1, store.settlements)
}

func TestControlReadinessRetainsAgentWireCode(t *testing.T) {
	p, _, _, _ := setup(t)
	require.NoError(t, p.EnableRestart())
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/missing/agent/control", strings.NewReader(`{}`)))
	require.Contains(t, response.Body.String(), `"code":"control_not_ready"`)
	require.NotContains(t, response.Body.String(), "Started not yet accepted")
}
