package controlplane

import (
	"context"
	"fmt"
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

func TestRebuildBoundsBlackHoledWorkerObservation(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) { testBlackHoledRebuild(t, n) })
	}
}
func testBlackHoledRebuild(t *testing.T, n int) {
	store := sessionstore.NewMemory()
	p := New(&fakeRelay{}, store)
	require.NoError(t, p.EnableRestart())
	lease, err := store.ClaimControl(context.Background(), "rebuild", 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	p.SetRelayReady(true)
	var reads atomic.Int32
	release := make(chan struct{})
	down := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer down.Close()
	defer close(release)
	address := netip.MustParseAddrPort("127.0.0.1:23495")
	liveAddress := netip.MustParseAddrPort("127.0.0.1:23496")
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: liveAddress, InventoryKnown: true})
	}))
	defer live.Close()
	fixed := []FixedWorker{{Name: "down", URL: down.URL, Address: address}, {Name: "live", URL: live.URL, Address: liveAddress}}
	for i := 1; i < n; i++ {
		fixed = append(fixed, FixedWorker{Name: fmt.Sprintf("down-%d", i), URL: down.URL, Address: netip.AddrPortFrom(address.Addr(), address.Port()+uint16(i+2))})
	}
	for _, worker := range fixed {
		require.NoError(t, p.controlRecord(context.Background(), "worker:"+worker.Name, worker))
		if worker.Name != "live" {
			require.NoError(t, p.controlRecord(context.Background(), "create:"+worker.Name, createIntent{Worker: worker.Name}))
		}
	}
	started := time.Now()
	require.NoError(t, p.rebuild(context.Background(), fixed))
	elapsed := time.Since(started)
	t.Logf("black_holed_workers=%d rebuild=%s status_reads=%d lease_plus_rebuild=%s default_rate_limit=2.2344s", n, elapsed, reads.Load(), ControlLeaseTTL+elapsed)
	require.Less(t, elapsed, 500*time.Millisecond, "one black-holed host must leave takeover headroom")
	require.EqualValues(t, n, reads.Load(), "discovery observation must be reused")
	p.mu.Lock()
	defer p.mu.Unlock()
	require.True(t, p.workers["down"].dead)
	require.False(t, p.workers["live"].dead)
}
