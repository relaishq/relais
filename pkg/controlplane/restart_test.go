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

type failingControlJournal struct {
	*sessionstore.Memory
	fail atomic.Bool
}

func (s *failingControlJournal) PutControlIntent(ctx context.Context, intent sessionstore.ControlIntent) error {
	if s.fail.Load() {
		return sessionstore.ErrTransient
	}
	return s.Memory.PutControlIntent(ctx, intent)
}
func (s *failingControlJournal) PutControlRecord(ctx context.Context, lease sessionstore.ControlLease, key string, data json.RawMessage) error {
	if s.fail.Load() {
		return sessionstore.ErrTransient
	}
	return s.Memory.PutControlRecord(ctx, lease, key, data)
}
func authorizeTestControl(t *testing.T, p *Plane) {
	t.Helper()
	require.NoError(t, p.EnableRestart())
	started := time.Now()
	lease, err := p.restart.store.ClaimControl(context.Background(), randomToken(), ControlLeaseTTL)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: started.Add(ControlLeaseTTL)})
	p.restart.healthy.Store(true)
	p.restart.ready.Store(true)
}
func TestControlRestartReadinessGatesEveryMutation(t *testing.T) {
	p, a, _, _ := setup(t)
	require.NoError(t, p.EnableRestart())
	ctx := context.Background()
	_, _, err := p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrNotReady)
	_, err = p.Move(ctx, "missing", "b")
	require.ErrorIs(t, err, ErrNotReady)
	require.ErrorIs(t, p.End(ctx, "missing"), ErrNotReady)
	_, err = p.Drain(ctx, "a")
	require.ErrorIs(t, err, ErrNotReady)
	require.ErrorIs(t, p.MarkDraining("a"), ErrNotReady)
	require.Zero(t, a.next)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.False(t, status.Ready)
	require.Equal(t, "waiting_lease", status.Rebuild.Phase)
	for _, request := range []struct{ method, path string }{{"POST", "/calls/missing/move"}, {"DELETE", "/calls/missing"}, {"POST", "/workers/a/drain"}, {"POST", "/private/workers/a/mark-draining"}} {
		response := httptest.NewRecorder()
		p.ProcessHandler().ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Contains(t, response.Body.String(), `"code":"control_not_ready"`)
	}
}
func TestControlIntentWriteFailurePreventsEffects(t *testing.T) {
	for _, operation := range []string{"move", "end", "create", "drain"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			p, a, _, _ := setup(t)
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			journal := &failingControlJournal{Memory: p.store.(*sessionstore.Memory)}
			p.store = journal
			authorizeTestControl(t, p)
			a.exported = make(chan struct{}, 1)
			initial := a.next
			journal.fail.Store(true)
			switch operation {
			case "move":
				_, err = p.Move(ctx, id, "b")
			case "end":
				err = p.End(ctx, id)
			case "create":
				_, _, err = p.Create(ctx, "offer", "a")
			case "drain":
				_, err = p.Drain(ctx, "a")
			}
			require.ErrorIs(t, err, sessionstore.ErrTransient)
			require.Equal(t, initial, a.next)
			require.True(t, a.runs(id))
			select {
			case <-a.exported:
				t.Fatal("export ran before durable intent")
			default:
			}
			p.mu.Lock()
			require.False(t, p.workers["a"].draining)
			p.mu.Unlock()
		})
	}
}
func TestControlExpiredLocalLeaseRefusesMutation(t *testing.T) {
	p, _, _, _ := setup(t)
	authorizeTestControl(t, p)
	guard := *p.restart.guard.Load()
	guard.deadline = time.Now().Add(-time.Second)
	p.restart.guard.Store(&guard)
	require.ErrorIs(t, p.End(context.Background(), "missing"), ErrNotReady)
	require.False(t, p.mutationReady())
}
func TestControlRebuildCrossChecksWorkerInventory(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "store-owner", true: "truncated"}[truncated], func(t *testing.T) {
			ctx := context.Background()
			store := sessionstore.NewMemory()
			address := netip.MustParseAddrPort("127.0.0.1:23456")
			lease, err := store.Claim(ctx, "installed", address, time.Minute)
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				privateapi.Write(w, mediaworker.WorkerStatus{Address: address, InventoryKnown: true, Truncated: truncated, Inventory: []mediaworker.InstalledSession{{ID: lease.SessionID, Owner: netip.MustParseAddrPort("127.0.0.1:29999"), Epoch: lease.Epoch + 1}}})
			}))
			defer server.Close()
			p := New(&fakeRelay{}, store)
			authorizeTestControl(t, p)
			p.SetRelayReady(true)
			err = p.rebuild(ctx, []FixedWorker{{Name: "a", URL: server.URL}})
			if truncated {
				require.ErrorContains(t, err, "truncated")
				return
			}
			require.NoError(t, err)
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Calls, 1)
			require.Equal(t, lease.Worker, status.Calls[0].Address)
			require.Equal(t, lease.Epoch, status.Calls[0].Epoch)
		})
	}
}
func TestControlUncertainCreateRetainsReservationUntilObserved(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	address := netip.MustParseAddrPort("127.0.0.1:23456")
	var unreachable atomic.Bool
	lease, err := store.Claim(ctx, "created", address, time.Minute)
	require.NoError(t, err)
	var creates atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates.Add(1)
			privateapi.Error(w, privateapi.Uncertain(errors.New("lost reply")), nil)
			return
		}
		if unreachable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		privateapi.Write(w, mediaworker.WorkerStatus{Address: address, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: lease.SessionID, Owner: address, Epoch: lease.Epoch}}})
	}))
	defer server.Close()
	p := New(&fakeRelay{}, store)
	authorizeTestControl(t, p)
	require.NoError(t, p.Register("a", address, &RemoteWorker{URL: server.URL}))
	p.mu.Lock()
	worker := p.workers["a"]
	worker.reserved++
	p.mu.Unlock()
	key, err := p.startCreate(ctx, worker)
	require.NoError(t, err)
	require.ErrorIs(t, p.finishCreate(ctx, key, worker, "", privateapi.Uncertain(errors.New("lost reply"))), privateapi.ErrUncertain)
	guard := p.restart.guard.Load()
	records, err := store.ControlRecords(ctx, guard.lease)
	require.NoError(t, err)
	require.Contains(t, records, key)
	unreachable.Store(true)
	require.NoError(t, p.reconcileCreates(ctx))
	p.mu.Lock()
	require.Equal(t, 1, worker.reserved)
	require.False(t, worker.dead)
	p.mu.Unlock()
	unreachable.Store(false)
	require.NoError(t, p.reconcileCreates(ctx))
	p.mu.Lock()
	require.Zero(t, worker.reserved)
	require.NotNil(t, p.calls[lease.SessionID])
	p.mu.Unlock()
	require.Zero(t, creates.Load(), "reconciliation must not replay create")
	current, err := store.Get(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, current.Epoch)
	records, err = store.ControlRecords(ctx, guard.lease)
	require.NoError(t, err)
	require.NotContains(t, records, key)
}

func TestControlRebuildAcceptsPriorCompletedHeartbeatToken(t *testing.T) {
	p, a, _, _ := setup(t)
	authorizeTestControl(t, p)
	_, err := p.remoteHeartbeat("a", HeartbeatRequest{Address: a.addr, Token: "prior-completed-handshake"})
	require.NoError(t, err)
	_, err = p.remoteHeartbeat("a", HeartbeatRequest{Address: a.addr, Token: "different-token"})
	require.Error(t, err)
	p.mu.Lock()
	p.workers["a"].dead = true
	p.workers["a"].recovered = true
	p.mu.Unlock()
	reply, err := p.remoteHeartbeat("a", HeartbeatRequest{Address: a.addr, Token: "prior-completed-handshake"})
	require.NoError(t, err)
	require.NotEmpty(t, reply.Token)
	require.NotEqual(t, "prior-completed-handshake", reply.Token)
	p.mu.Lock()
	require.True(t, p.workers["a"].dead)
	p.mu.Unlock()
}
func TestControlTerminalCleanupClearsDurableIntent(t *testing.T) {
	ctx := context.Background()
	p, _, _, _ := setup(t)
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	authorizeTestControl(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NoError(t, p.beginIntent(ctx, c, "takeover", lease))
	c.terminal = true
	p.lose(c, lease, false)
	p.cleanupExpired(ctx, c)
	intents, err := p.restart.store.ControlIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, intents)
	_, err = p.lookup(id)
	require.ErrorIs(t, err, ErrUnknownCall)
}

type lostCreateClear struct {
	*sessionstore.Memory
	drop atomic.Bool
}

func (s *lostCreateClear) PutControlRecord(ctx context.Context, lease sessionstore.ControlLease, key string, data json.RawMessage) error {
	if err := s.Memory.PutControlRecord(ctx, lease, key, data); err != nil {
		return err
	}
	if len(data) == 0 && s.drop.CompareAndSwap(true, false) {
		return sessionstore.ErrTransient
	}
	return nil
}
func TestControlCreateLostClearReplyKeepsSuccess(t *testing.T) {
	ctx := context.Background()
	p, a, _, _ := setup(t)
	store := &lostCreateClear{Memory: p.store.(*sessionstore.Memory)}
	p.store = store
	authorizeTestControl(t, p)
	store.drop.Store(true)
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	require.True(t, a.runs(id))
	require.Zero(t, p.workers["a"].reserved)
	require.NoError(t, p.reconcileCreates(ctx))
	require.NotNil(t, p.calls[id])
	require.Empty(t, p.restart.creates)
}
