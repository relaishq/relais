package controlplane

import (
	"context"
	"errors"
	"github.com/relais/internal/relayleg"
	"github.com/relais/pkg/relay"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type retainedMoveStore struct {
	sessionstore.Store
	failRead bool
}

func (s *retainedMoveStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	candidate, err := s.Store.Transfer(ctx, from, to, ttl)
	if err != nil {
		return candidate, err
	}
	s.failRead = true
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "transfer", Err: errors.New("lost reply"), Candidate: &candidate}
}
func (s *retainedMoveStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.failRead {
		s.failRead = false
		return sessionstore.Lease{}, &sessionstore.TransientError{Op: "get", Err: errors.New("offline")}
	}
	return s.Store.Get(ctx, id)
}
func (*retainedMoveStore) Settle(context.Context, sessionstore.Lease) (sessionstore.Lease, bool, error) {
	return sessionstore.Lease{}, false, &sessionstore.TransientError{Op: "settle", Err: errors.New("offline")}
}

type trackedHoldRelay struct {
	*fakeRelay
	releases []netip.AddrPort
}

func (r *trackedHoldRelay) ReleaseSession(_ string, to netip.AddrPort) (int, error) {
	r.releases = append(r.releases, to)
	return 7, nil
}
func TestRetainedPlannedMoveKeepsHoldAndExactExport(t *testing.T) {
	p, a, b, r := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	hold := &trackedHoldRelay{fakeRelay: r}
	p.relay = hold
	p.store = &retainedMoveStore{Store: p.store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	require.False(t, a.runs(id))
	require.Empty(t, hold.releases, "exported source must not receive released packets")
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.takeover(ctx, source, pending.lease, time.Now())
	require.True(t, b.runs(id), "final export needs no crash counter margin")
	require.Equal(t, []netip.AddrPort{b.addr}, hold.releases)
	require.Empty(t, source.pending)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "move", status.Calls[0].LastMoveKind)
	require.EqualValues(t, 1, status.Calls[0].MoveCount)
	require.Zero(t, status.Calls[0].TakeoverCount)
	require.Empty(t, status.Takeovers)
}

type gatedPlannedRead struct {
	sessionstore.Store
	id      string
	blocked atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *gatedPlannedRead) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if id == s.id && s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return s.Store.Get(ctx, id)
}
func TestDeathDetectionContinuesDuringPlannedRetry(t *testing.T) {
	p, a, baseB, _ := setup(t)
	p.workers["b"].worker = &takeoverWorker{fakeWorker: baseB}
	ctx := context.Background()
	planned, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	remaining, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	original, err := p.store.Get(ctx, planned)
	require.NoError(t, err)
	candidate, err := p.store.Transfer(ctx, original, baseB.addr, time.Minute)
	require.NoError(t, err)
	// Both are valid exported snapshots; only the remaining session is stale.
	remainingLease, err := p.store.Get(ctx, remaining)
	require.NoError(t, err)
	require.NoError(t, p.store.PutState(ctx, remainingLease, takeoverSnapshot(t, remaining, 0)))
	source := p.workers["a"]
	source.pending = map[string]*takeoverState{planned: {lease: original, candidate: &candidate, planned: true, plannedState: takeoverSnapshot(t, planned, 0), routed: a.addr, excluded: map[netip.AddrPort]bool{a.addr: true}, attemptLimit: 3}}
	gate := &gatedPlannedRead{Store: p.store, id: planned, entered: make(chan struct{}), release: make(chan struct{})}
	p.store = gate
	p.config.DeadAfter = 80 * time.Millisecond
	p.config.CheckInterval = 5 * time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Run(runCtx) }()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate.release) }); cancel(); require.NoError(t, <-done) })
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				_ = p.Heartbeat(baseB.addr)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-hbDone })
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("planned retry did not enter")
	}
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return source.dead && source.retryingMoves }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return baseB.runs(remaining) }, time.Second, time.Millisecond, "remaining session recovers while planned retry is blocked")
	once.Do(func() { close(gate.release) })
	require.Eventually(t, func() bool { return baseB.runs(planned) }, time.Second, time.Millisecond)
}

// A lost final export must immediately free a real relay's bounded hold slot.
func TestLostRetainedMoveReleasesRelayHold(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store, MaxHeldSessions: 1, HoldTimeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	listen := func() *net.UDPConn {
		conn, listenErr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(t, listenErr)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })
		return conn
	}
	connA, connB := listen(), listen()
	a := &fakeWorker{store: store, addr: connA.LocalAddr().(*net.UDPAddr).AddrPort(), running: map[string]bool{}}
	b := &fakeWorker{store: store, addr: connB.LocalAddr().(*net.UDPAddr).AddrPort(), running: map[string]bool{}}
	p := New(r, store)
	for name, w := range map[string]*fakeWorker{"a": a, "b": b} {
		r.AddWorker(w.addr)
		require.NoError(t, p.Register(name, w.addr, w))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acked := make(chan error, 1)
	go func() {
		_ = connA.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, _, readErr := connA.ReadFromUDPAddrPort(buf)
		if readErr == nil {
			nonce, _, valid := relayleg.ParseBarrier(buf[:n])
			if !valid {
				readErr = relay.ErrMalformedHeader
			} else {
				_, readErr = connA.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), r.WorkerAddr())
			}
		}
		acked <- readErr
	}()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.store = &retainedMoveStore{Store: store}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	require.NoError(t, <-acked)
	require.Equal(t, 1, r.Stats().Holds)
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.workers["b"].draining = true
	p.takeover(ctx, source, pending.lease, time.Now())
	require.Zero(t, r.Stats().Holds, "loss frees MaxHeldSessions without waiting 30 seconds")
	require.Zero(t, r.Stats().HoldTimeouts)
	require.Empty(t, source.pending)
	_, err = store.Get(ctx, id)
	require.ErrorIs(t, err, sessionstore.ErrNotFound)
}

type reverseReplyLostStore struct {
	*retainedMoveStore
	source netip.AddrPort
}

func (s *reverseReplyLostStore) Transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	if to == s.source {
		return s.retainedMoveStore.Transfer(ctx, from, to, ttl)
	}
	return s.Store.Transfer(ctx, from, to, ttl)
}

type failedRouteRelay struct {
	*fakeRelay
	route       netip.AddrPort
	target      netip.AddrPort
	failForward bool
}

func (r *failedRouteRelay) MoveSession(id string, from, to netip.AddrPort) error {
	if r.failForward && to == r.target {
		r.failForward = false
		return errors.New("failed route update before mutation")
	}
	if from != r.route {
		return errors.New("retry used a route that was never installed")
	}
	r.route = to
	return r.fakeRelay.MoveSession(id, from, to)
}
func TestUncertainRollbackAfterFailedRouteRetriesActualSource(t *testing.T) {
	p, a, b, r := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	routes := &failedRouteRelay{fakeRelay: r, route: a.addr, target: b.addr, failForward: true}
	p.relay = routes
	p.store = &reverseReplyLostStore{retainedMoveStore: &retainedMoveStore{Store: p.store}, source: a.addr}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	require.False(t, a.runs(id))
	require.Equal(t, a.addr, routes.route)
	source := p.workers["a"]
	pending := source.pending[id]
	require.NotNil(t, pending)
	p.retryPlannedMoves(ctx, source, time.Now())
	require.True(t, a.runs(id), "retry resumes the source after its reverse tenure is adopted")
	require.Equal(t, a.addr, routes.route)
	require.Empty(t, source.pending)
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "a", status.Calls[0].Owner)
	require.Equal(t, "move", status.Calls[0].LastMoveKind)
}
