package mediaworker

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type indexRefreshRequest struct {
	worker        netip.AddrPort
	ttl, timeLeft time.Duration
}

type blockedFirstRenew struct {
	refreshed  chan indexRefreshRequest
	indexCalls atomic.Int32
	sessionstore.Store
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	renewed chan string
}

func (s *blockedFirstRenew) Renew(ctx context.Context, lease sessionstore.Lease, ttl time.Duration) (sessionstore.Lease, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	s.renewed <- lease.SessionID
	return lease, nil
}
func (s *blockedFirstRenew) RefreshWorkerIndex(ctx context.Context, worker netip.AddrPort, ttl time.Duration) error {
	s.indexCalls.Add(1)
	deadline, _ := ctx.Deadline()
	if s.refreshed != nil {
		select {
		case s.refreshed <- indexRefreshRequest{worker: worker, ttl: ttl, timeLeft: time.Until(deadline)}:
		default:
		}
	}
	return nil
}
func TestSlowRenewDoesNotSerializeOtherSessions(t *testing.T) {
	store := &blockedFirstRenew{Store: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{}), renewed: make(chan string, 100), refreshed: make(chan indexRefreshRequest, 1)}
	w := &Worker{localAddr: netip.MustParseAddrPort("127.0.0.1:1"), cfg: Config{Relay: &RelayConfig{Owners: store, LeaseTTL: 30 * time.Millisecond}}, stopRenew: make(chan struct{}), sessions: make(map[string]*session), log: logging.NewDefaultLoggerFactory().NewLogger("renew-test")}
	for i := range 40 {
		id := fmt.Sprint(i)
		w.sessions[id] = &session{id: id, ctx: context.Background(), lease: sessionstore.Lease{SessionID: id, Epoch: 1, Worker: netip.MustParseAddrPort("127.0.0.1:1")}}
	}
	w.running.Add(1)
	go w.renewLeases()
	t.Cleanup(func() { close(w.stopRenew); close(store.release); w.running.Wait() })
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("first renewal did not enter")
	}
	// Index refresh runs once even while the first lease renewal is blocked.
	select {
	case request := <-store.refreshed:
		require.Equal(t, w.localAddr, request.worker)
		require.Equal(t, w.cfg.Relay.LeaseTTL, request.ttl)
		require.Positive(t, request.timeLeft)
		require.LessOrEqual(t, request.timeLeft, ownershipTimeout)
	case <-time.After(time.Second):
		t.Fatal("renewal tick did not refresh the worker index")
	}
	require.EqualValues(t, 1, store.indexCalls.Load())
	// One round trip is still blocked, while the other 39 sessions renew through
	// the production loop, including sessions beyond the concurrency limit.
	deadline := time.After(300 * time.Millisecond)
	seen := map[string]bool{}
	for len(seen) < 39 {
		select {
		case id := <-store.renewed:
			seen[id] = true
		case <-deadline:
			t.Fatal("slow renewal serialized healthy sessions")
		}
	}
}

type snapshotGateStore struct {
	sessionstore.Store
	mu      sync.Mutex
	id      string
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (s *snapshotGateStore) PutState(ctx context.Context, lease sessionstore.Lease, state []byte) error {
	s.mu.Lock()
	blocked := lease.SessionID == s.id && s.id != ""
	if blocked {
		s.calls++
		if s.calls == 1 {
			close(s.entered)
		}
	}
	s.mu.Unlock()
	if blocked {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.PutState(ctx, lease, state)
}
func TestSnapshotsConcurrentAcrossSessionsAndOrderedWithinSession(t *testing.T) {
	store := &snapshotGateStore{Store: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	w, err := New(Config{SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
	require.NoError(t, err)
	r.AddWorker(w.LocalAddr())
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	first, _ := dialDTLSCaller(t, w)
	second, _ := dialDTLSCaller(t, w)
	require.Eventually(t, func() bool {
		return w.session(first.id).snapshotStored.Load() && w.session(second.id).snapshotStored.Load()
	}, time.Second, time.Millisecond)
	store.mu.Lock()
	store.id = first.id
	store.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(release)
	initial := make(chan error, 1)
	go func() { initial <- w.session(first.id).persistSnapshot() }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("blocked snapshot did not enter")
	}
	concurrent := make(chan error, 1)
	go func() { concurrent <- w.session(second.id).persistSnapshot() }()
	select {
	case err := <-concurrent:
		require.NoError(t, err)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("one snapshot blocked a different session")
	}
	ordered := make(chan error, 1)
	go func() { ordered <- w.session(first.id).persistSnapshot() }()
	select {
	case <-ordered:
		t.Fatal("second snapshot bypassed per-session ordering")
	case <-time.After(30 * time.Millisecond):
	}
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	require.Equal(t, 1, calls, "second copy/put waits behind first")
	release()
	require.NoError(t, <-initial)
	require.NoError(t, <-ordered)
}

type uncertainClaimStore struct {
	sessionstore.Store
	entered chan struct{}
	release chan struct{}
}

func (s *uncertainClaimStore) Claim(ctx context.Context, id string, worker netip.AddrPort, ttl time.Duration) (sessionstore.Lease, error) {
	candidate, err := s.Store.Claim(ctx, id, worker, ttl)
	if err != nil {
		return candidate, err
	}
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "claim", Err: fmt.Errorf("lost committed reply"), Candidate: &candidate}
}
func (s *uncertainClaimStore) Settle(ctx context.Context, c sessionstore.Lease) (sessionstore.Lease, bool, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return sessionstore.Lease{}, false, ctx.Err()
	}
	lease, err := s.Get(ctx, c.SessionID)
	return lease, err == nil && lease.Epoch == c.Epoch, err
}
func TestFailedClaimCandidateIsCleanedUpInBackground(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprint(successor), func(t *testing.T) {
			store := &uncertainClaimStore{Store: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
			w := &Worker{cfg: Config{Relay: &RelayConfig{Owners: store, LeaseTTL: time.Minute}}, localAddr: netip.MustParseAddrPort("127.0.0.1:1"), log: logging.NewDefaultLoggerFactory().NewLogger("claim-test")}
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(store.release) }); w.running.Wait() })
			require.Error(t, w.claim(context.Background(), &session{id: "failed-claim"}))
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("candidate cleanup did not start")
			}
			lease, err := store.Get(context.Background(), "failed-claim")
			require.NoError(t, err)
			if successor {
				_, err = store.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:2"), time.Minute)
				require.NoError(t, err)
			}
			once.Do(func() { close(store.release) })
			w.running.Wait()
			current, err := store.Get(context.Background(), "failed-claim")
			if successor {
				require.NoError(t, err)
				require.Greater(t, current.Epoch, lease.Epoch)
			} else {
				require.ErrorIs(t, err, sessionstore.ErrNotFound)
			}
		})
	}
}
