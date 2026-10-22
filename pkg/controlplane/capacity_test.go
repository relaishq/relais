package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestWorkerCapAdmissionAndEnd(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 2
	ctx := context.Background()
	var ids []string
	for range 4 {
		id, _, err := p.Create(ctx, "offer", "")
		require.NoError(t, err)
		ids = append(ids, id)
	}
	_, _, err := p.Create(ctx, "offer", "")
	require.ErrorIs(t, err, ErrAtCapacity)
	require.NoError(t, p.End(ctx, ids[0]))
	_, _, err = p.Create(ctx, "offer", "")
	require.NoError(t, err)
	require.NoError(t, p.MarkDraining("a"))
	require.NoError(t, p.MarkDraining("b"))
	_, _, err = p.Create(ctx, "offer", "")
	require.ErrorIs(t, err, ErrNoTarget)
}

type pendingCreateWorker struct {
	*fakeWorker
	once             sync.Once
	started, release chan struct{}
}

func (w *pendingCreateWorker) CreateSession(ctx context.Context, offer string) (string, string, error) {
	blocked := false
	w.once.Do(func() { blocked = true; close(w.started) })
	if blocked {
		select {
		case <-w.release:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	return w.fakeWorker.CreateSession(ctx, offer)
}

func TestWorkerCapCountsReservations(t *testing.T) {
	p, a, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	gate := &pendingCreateWorker{fakeWorker: a, started: make(chan struct{}), release: make(chan struct{})}
	p.workers["a"].worker = gate
	done := make(chan error, 1)
	go func() { _, _, err := p.Create(context.Background(), "offer", "a"); done <- err }()
	<-gate.started
	defer func() { close(gate.release); require.NoError(t, <-done) }()
	_, _, err := p.Create(context.Background(), "offer", "a")
	require.ErrorIs(t, err, ErrAtCapacity)
	require.NoError(t, p.SamplePool(context.Background()))
	p.mu.Lock()
	sample := p.poolLocked(time.Now())
	p.mu.Unlock()
	require.Equal(t, 1, sample.Reservations)
	require.Equal(t, 1, *sample.Demand)
}

// The store returns an old list after another create publishes its lease and
// releases its reservation. The old list must not admit one more call.
type capturedLoadStore struct {
	sessionstore.Store
	once              atomic.Bool
	captured, release chan struct{}
}

func (s *capturedLoadStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	leases, err := s.Store.ListByWorker(ctx, addr)
	if s.once.CompareAndSwap(false, true) {
		close(s.captured)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return leases, err
}
func TestWorkerCapRechecksSettledReservation(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	store := &capturedLoadStore{Store: p.store, captured: make(chan struct{}), release: make(chan struct{})}
	p.store = store
	done := make(chan error, 1)
	go func() { _, _, err := p.Create(context.Background(), "offer", "a"); done <- err }()
	<-store.captured
	_, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	close(store.release)
	require.ErrorIs(t, <-done, ErrAtCapacity)
}

func TestWorkerCapConcurrentCreates(t *testing.T) {
	p, a, b, _ := setup(t)
	p.config.WorkerCallCap = 3
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 96 {
		wg.Go(func() {
			_, _, err := p.Create(context.Background(), "offer", "")
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrAtCapacity) {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
	wg.Wait()
	// A lease published before its create reply can conservatively count
	// alongside its still-live reservation. Once requests settle, all six
	// slots must be usable, and the next create must be refused.
	require.LessOrEqual(t, successes.Load(), int64(6))
	for accepted := successes.Load(); accepted < 6; accepted++ {
		_, _, err := p.Create(context.Background(), "offer", "")
		require.NoError(t, err)
	}
	_, _, err := p.Create(context.Background(), "offer", "")
	require.ErrorIs(t, err, ErrAtCapacity)
	for _, w := range []*fakeWorker{a, b} {
		leases, err := p.store.ListByWorker(context.Background(), w.addr)
		require.NoError(t, err)
		require.Len(t, leases, 3)
	}
}

func TestWorkerCapPlannedMoveAndDrainStayPut(t *testing.T) {
	p, a, b, _ := setup(t)
	p.config.WorkerCallCap = 1
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	_, _, err = p.Create(ctx, "offer", "b")
	require.NoError(t, err)
	_, err = p.Move(ctx, id, "b")
	require.ErrorIs(t, err, ErrAtCapacity)
	moves, err := p.Drain(ctx, "a")
	require.ErrorIs(t, err, ErrAtCapacity)
	require.Empty(t, moves)
	require.True(t, a.runs(id))
	require.False(t, b.runs(id))
	leases, err := p.store.ListByWorker(ctx, b.addr)
	require.NoError(t, err)
	require.Len(t, leases, 1)
}

func TestWorkerCapTakeoverExceedsBudget(t *testing.T) {
	p, _, b, _ := setup(t)
	p.config.WorkerCallCap = 1
	p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	_, _, err = p.Create(ctx, "offer", "b")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	expireObservedWorker(t, p.workers["a"])
	p.takeover(ctx, p.workers["a"], lease, time.Now())
	require.True(t, b.runs(id))
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Pool.OverCapCalls)
	require.Equal(t, 2, *status.Pool.Demand)
	require.Contains(t, planeMetricsBody(p), "relais_control_pool_over_cap_calls 1")
}

func TestWorkerCapDisabled(t *testing.T) {
	p, _, _, _ := setup(t)
	for range 20 {
		_, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
	}
}

func TestWorkerCapHTTPRefusal(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	_, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	for _, handler := range []http.Handler{p.Handler(), p.ProcessHandler()} {
		r := httptest.NewRequest(http.MethodPost, "/calls?worker=a", strings.NewReader("offer"))
		r.Header.Set("Content-Type", "application/sdp")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		require.Equal(t, 503, w.Code)
		require.Equal(t, "1", w.Header().Get("Retry-After"))
		var failure map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &failure))
		require.Equal(t, "at_capacity", failure["code"])
	}
}

func TestPoolSampleMissingStaleAndRefreshFailure(t *testing.T) {
	p, a, _, _ := setup(t)
	body := planeMetricsBody(p)
	require.Contains(t, body, "relais_control_pool_sample_valid 0")
	require.NotContains(t, body, "\nrelais_control_pool_demand ")
	_, err := p.store.Claim(context.Background(), "existing", a.addr, time.Minute)
	require.NoError(t, err)
	require.NoError(t, p.SamplePool(context.Background()))
	p.mu.Lock()
	p.pool.SampledAt = time.Now().Add(-time.Minute)
	sample := p.poolLocked(time.Now())
	p.mu.Unlock()
	require.True(t, sample.Stale)
	require.Equal(t, 1, *sample.Demand)
	require.Greater(t, sample.AgeSeconds, 59.0)
	body = planeMetricsBody(p)
	require.Contains(t, body, "relais_control_pool_sample_stale 1")
	require.Contains(t, body, "relais_control_pool_demand 1")
	store := &flakyListStore{Store: p.store}
	store.fail.Store(true)
	p.store = store
	status, err := p.Status(context.Background())
	require.NoError(t, err)
	require.True(t, status.Pool.Stale)
	require.Equal(t, 1, *status.Pool.Demand)
	store.fail.Store(false)
	status, err = p.Status(context.Background())
	require.NoError(t, err)
	require.False(t, status.Pool.Stale)
	require.Equal(t, 2, status.Pool.Ready)
	p.BeginBootstrap([]string{"missing"})
	status, err = p.Status(context.Background())
	require.NoError(t, err)
	require.Nil(t, status.Pool.Demand)
	require.True(t, status.Pool.Stale)
	require.Equal(t, 1, status.Pool.Pending)
}

type rollbackGateRelay struct {
	*fakeRelay
	entered, release chan struct{}
	once             sync.Once
}

func (r *rollbackGateRelay) MoveSession(id string, from, to netip.AddrPort) error {
	fail := false
	r.once.Do(func() { close(r.entered); <-r.release; fail = true })
	if fail {
		return errors.New("route unavailable")
	}
	return r.fakeRelay.MoveSession(id, from, to)
}
func TestWorkerCapKeepsRollbackSlot(t *testing.T) {
	p, a, b, r := setup(t)
	p.config.WorkerCallCap = 1
	gate := &rollbackGateRelay{fakeRelay: r, entered: make(chan struct{}), release: make(chan struct{})}
	p.relay = gate
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := p.Move(context.Background(), id, "b"); done <- err }()
	<-gate.entered
	_, _, err = p.Create(context.Background(), "offer", "a")
	close(gate.release)
	require.ErrorIs(t, err, ErrAtCapacity)
	require.ErrorContains(t, <-done, "rolled back")
	require.True(t, a.runs(id))
	require.False(t, b.runs(id))
	leases, err := p.store.ListByWorker(context.Background(), a.addr)
	require.NoError(t, err)
	require.Len(t, leases, 1)
}

type unavailableObservationStore struct{ sessionstore.Store }

func (s unavailableObservationStore) Get(context.Context, string) (sessionstore.Lease, error) {
	return sessionstore.Lease{}, sessionstore.ErrTransient
}
func (s unavailableObservationStore) ListByWorker(context.Context, netip.AddrPort) ([]sessionstore.Lease, error) {
	return nil, sessionstore.ErrTransient
}
func TestPoolSampleAvailableDuringOwnershipOutage(t *testing.T) {
	p, _, _, _ := setup(t)
	_, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	require.NoError(t, p.SamplePool(context.Background()))
	p.store = unavailableObservationStore{p.store}
	reply := httptest.NewRecorder()
	p.Handler().ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/status", nil))
	require.Equal(t, 500, reply.Code)
	var body struct {
		Ready bool
		Pool  PoolSample
		Error string
	}
	require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &body))
	require.False(t, body.Ready)
	require.NotEmpty(t, body.Error)
	require.True(t, body.Pool.Stale)
	require.Equal(t, 1, *body.Pool.Demand)
}

func TestPoolSampleStatusAndMetricsFields(t *testing.T) {
	p, _, _, _ := setup(t)
	p.config.WorkerCallCap = 1
	for range 3 {
		_, _, _ = p.Create(context.Background(), "offer", "")
	}
	_, err := p.Status(context.Background())
	require.NoError(t, err)
	body := planeMetricsBody(p)
	for _, name := range []string{
		"relais_control_pool_demand", "relais_control_worker_call_cap", "relais_control_pool_workers",
		"relais_control_pool_reservations", "relais_control_refusals_total", "relais_control_pool_over_cap_calls",
		"relais_control_pool_sample_timestamp_seconds", "relais_control_pool_sample_age_seconds",
		"relais_control_pool_sample_valid", "relais_control_pool_sample_stale", "relais_control_active_calls",
	} {
		require.Contains(t, body, "\n"+name)
	}
	require.Contains(t, body, `relais_control_refusals_total{reason="at_capacity"} 1`)
	reply := httptest.NewRecorder()
	p.Handler().ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/status", nil))
	var status struct{ Pool map[string]json.RawMessage }
	require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &status))
	for _, field := range []string{"demand", "worker_call_cap", "ready_workers", "draining_workers", "unreachable_workers", "pending_workers", "reservations", "refusals", "over_cap_calls", "sampled_at", "age_seconds", "stale"} {
		require.Contains(t, status.Pool, field)
	}
}

func TestWorkerCapRetainedMoveAtBudget(t *testing.T) {
	p, a, b, r := setup(t)
	p.config.WorkerCallCap = 1
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	hold := &trackedHoldRelay{fakeRelay: r}
	p.relay = hold
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	require.False(t, a.runs(id))
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.takeover(ctx, source, pending.lease, time.Now())
	require.True(t, b.runs(id), "the retained lease already consumes the destination slot")
	require.Empty(t, source.pending)
	leases, err := p.store.ListByWorker(ctx, b.addr)
	require.NoError(t, err)
	require.Len(t, leases, 1)
}
