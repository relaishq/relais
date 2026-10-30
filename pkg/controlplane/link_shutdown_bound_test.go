package controlplane

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type unresponsiveDrainStore struct {
	*sessionstore.Memory
	hold             atomic.Bool
	once             sync.Once
	entered, release chan struct{}
}

func (s *unresponsiveDrainStore) ListByWorker(ctx context.Context, addr netip.AddrPort) ([]sessionstore.Lease, error) {
	if s.hold.Load() {
		s.once.Do(func() { close(s.entered) })
		// Ignore cancellation, but keep a broken test from leaking forever.
		select {
		case <-s.release:
		case <-time.After(15 * time.Second):
			return nil, context.DeadlineExceeded
		}
	}
	return s.Memory.ListByWorker(ctx, addr)
}

func TestShutdownBoundsDrainWaitBeforeClosingLink(t *testing.T) {
	store := &unresponsiveDrainStore{Memory: sessionstore.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newPipeAdmissionRig(t, noContentHandler(), store)
	stream, err := r.open(t, "A", 1, 0, r.addr)
	require.NoError(t, err)
	peer := stream.run(statusHandler())
	store.hold.Store(true)
	defer close(store.release)
	require.NoError(t, reportLeg(context.Background(), &http.Client{Transport: peer}, netip.MustParseAddrPort("127.0.0.1:17002")))
	receiveLinkTest(t, store.entered, "blocked drain store")
	started := time.Now()
	stopped := make(chan struct{})
	go func() { r.p.BeginShutdown(); close(stopped) }()
	select {
	case <-peer.Done():
		require.GreaterOrEqual(t, time.Since(started), 4*time.Second, "shutdown dropped the link before waiting for work")
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not bound its wait for an unresponsive dependency")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not return after closing the streams")
	}
}
