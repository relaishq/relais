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
		<-s.release // Simulate a dependency that does not honor cancellation.
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
	<-store.entered
	started := time.Now()
	stopped := make(chan struct{})
	go func() { r.p.BeginShutdown(); close(stopped) }()
	select {
	case <-peer.Done():
		require.GreaterOrEqual(t, time.Since(started), 4*time.Second, "shutdown dropped the link before waiting for work")
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown did not bound its wait for an unresponsive dependency")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after closing the streams")
	}
}
