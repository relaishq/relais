package controlplane_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// This adapter is installed only on A. Every runtime ownership/snapshot
// operation fails before reaching the real store while the partition is armed.
type partitionedStore struct {
	sessionstore.Store
	blocked  atomic.Bool
	failures atomic.Uint64
}

func (s *partitionedStore) unavailable() error {
	if s.blocked.Load() {
		s.failures.Add(1)
		return &sessionstore.TransientError{Op: "worker partition", Err: errors.New("store unreachable")}
	}
	return nil
}
func (s *partitionedStore) Clock(c context.Context, id string) (time.Time, error) {
	if e := s.unavailable(); e != nil {
		return time.Time{}, e
	}
	return s.Store.Clock(c, id)
}
func (s *partitionedStore) PutState(c context.Context, l sessionstore.Lease, b []byte) error {
	if e := s.unavailable(); e != nil {
		return e
	}
	return s.Store.PutState(c, l, b)
}
func (s *partitionedStore) Renew(c context.Context, l sessionstore.Lease, d time.Duration) (sessionstore.Lease, error) {
	if e := s.unavailable(); e != nil {
		return sessionstore.Lease{}, e
	}
	return s.Store.Renew(c, l, d)
}
func (s *partitionedStore) Claim(c context.Context, id string, a netip.AddrPort, d time.Duration) (sessionstore.Lease, error) {
	if e := s.unavailable(); e != nil {
		return sessionstore.Lease{}, e
	}
	return s.Store.Claim(c, id, a, d)
}
func (s *partitionedStore) Get(c context.Context, id string) (sessionstore.Lease, error) {
	if e := s.unavailable(); e != nil {
		return sessionstore.Lease{}, e
	}
	return s.Store.Get(c, id)
}
func (s *partitionedStore) GetState(c context.Context, id string) ([]byte, error) {
	if e := s.unavailable(); e != nil {
		return nil, e
	}
	return s.Store.GetState(c, id)
}
func (s *partitionedStore) Checkpoint(c context.Context, id string, b []byte) (sessionstore.Checkpoint, error) {
	if e := s.unavailable(); e != nil {
		return sessionstore.Checkpoint{}, e
	}
	return s.Store.Checkpoint(c, id, b)
}
func (s *partitionedStore) Release(c context.Context, l sessionstore.Lease) error {
	if e := s.unavailable(); e != nil {
		return e
	}
	return s.Store.Release(c, l)
}

func (s *partitionedStore) Owner(c context.Context, id string) (netip.AddrPort, error) {
	if e := s.unavailable(); e != nil {
		return netip.AddrPort{}, e
	}
	return s.Store.Owner(c, id)
}
func (s *partitionedStore) Transfer(c context.Context, l sessionstore.Lease, a netip.AddrPort, d time.Duration) (sessionstore.Lease, error) {
	if e := s.unavailable(); e != nil {
		return sessionstore.Lease{}, e
	}
	return s.Store.Transfer(c, l, a, d)
}
func (s *partitionedStore) ListByWorker(c context.Context, a netip.AddrPort) ([]sessionstore.Lease, error) {
	if e := s.unavailable(); e != nil {
		return nil, e
	}
	return s.Store.ListByWorker(c, a)
}

type partitionWire struct {
	net.PacketConn
	mu       sync.Mutex
	received map[string]bool
}

func (c *partitionWire) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, e := c.PacketConn.ReadFrom(b)
	if e == nil {
		c.mu.Lock()
		c.received[string(b[:n])] = true
		c.mu.Unlock()
	}
	return n, a, e
}

// A loses store access and heartbeats but retains its media loop and keys.
// Real detector recovery transfers to B before expiry. Deliberately stale
// encrypted output from A is checked at the caller's raw socket, so passing
// cannot be explained by caller decryption rejecting it.
func TestPartitionedWorkerStoreTransferHasExclusiveCallerMedia(t *testing.T) {
	forHTTPRecoveryStores(t, func(t *testing.T, store sessionstore.Store) {
		aStore := &partitionedStore{Store: store}
		conn, e := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, e)
		wire := &partitionWire{PacketConn: conn, received: map[string]bool{}}
		s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{store: store, workerStores: []sessionstore.Store{aStore}, skipSourceHeartbeats: true, deadAfter: 1200 * time.Millisecond, callerSocket: func() (net.PacketConn, error) { return wire, nil }})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, e := s.harness.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
		require.NoError(t, e)
		media, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(media, 7*time.Second) }()
		defer func() { stop(); <-sent }()
		time.Sleep(300 * time.Millisecond)
		old, e := store.Get(ctx, call.SessionID())
		require.NoError(t, e)
		zombie, e := workerprobe.Zombie(s.workers[0].LocalAddr(), call.SessionID())
		require.NoError(t, e)
		aStore.blocked.Store(true)
		require.Eventually(t, func() bool {
			st, e := s.plane.Status(ctx)
			return e == nil && len(st.Calls) == 1 && st.Calls[0].Owner == "1" && len(st.Takeovers) == 1 && st.Takeovers[0].Error == "" && !st.Takeovers[0].Lost
		}, 2*time.Second, 5*time.Millisecond)
		current, e := store.Get(ctx, call.SessionID())
		require.NoError(t, e)
		require.Greater(t, current.Epoch, old.Epoch)
		require.True(t, time.Now().Before(old.ExpiresAt), "CAS takeover must precede old expiry")
		require.Equal(t, 1, s.workers[0].SessionCount(), "transient failures must leave A's live session running")
		require.Positive(t, aStore.failures.Load())
		var stale [][]byte
		for range 40 {
			b, e := zombie([]byte("PARTITIONED-WORKER-A"))
			require.NoError(t, e)
			stale = append(stale, b)
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		wire.mu.Lock()
		accepted := 0
		for _, b := range stale {
			if wire.received[string(b)] {
				accepted++
			}
		}
		wire.mu.Unlock()
		require.Zero(t, accepted, "stale A ciphertext reached caller while B owned the route")
		// Restore A's access. Its next renewal/checkpoint observes B's epoch and
		// fences; it must never silently re-claim or change the relay back to A.
		aStore.blocked.Store(false)
		require.Eventually(t, func() bool { return s.workers[0].SessionCount() == 0 }, 2*time.Second, 5*time.Millisecond)
		_, e = s.workers[0].SessionDecryptFailures(call.SessionID())
		require.ErrorIs(t, e, mediaworker.ErrUnknownSession)
		until := time.Now()
		report, e := call.Hangup(ctx)
		require.NoError(t, e)
		require.Zero(t, report.DecryptionFailures.Total())
		require.True(t, report.ConnectedThroughout())
		require.Zero(t, report.ICERestarts)
		require.Zero(t, report.Renegotiations)
		for _, tr := range report.Tracks {
			require.Less(t, until.Sub(report.StartedAt.Add(tr.LastArrival)), 200*time.Millisecond, "B media must keep flowing")
		}
		t.Logf("PARTITION_TRANSFER old_epoch=%d new_epoch=%d A_store_failures=%d stale_packets=%d caller_received=%d decrypt=%d", old.Epoch, current.Epoch, aStore.failures.Load(), len(stale), accepted, report.DecryptionFailures.Total())
	})
}
