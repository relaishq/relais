package controlplane_test

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// udpNAT preserves the worker-facing endpoint while replacing the relay-facing
// mapping. Closing the old mapping also removes its return path mid-call.
type udpNAT struct {
	front  *net.UDPConn
	relay  netip.AddrPort
	mu     sync.Mutex
	back   *net.UDPConn
	worker netip.AddrPort
	done   sync.WaitGroup
}

func newUDPNAT(t *testing.T, relay netip.AddrPort) *udpNAT {
	t.Helper()
	front, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	n := &udpNAT{front: front, relay: relay}
	n.rebind(t)
	n.done.Go(func() {
		b := make([]byte, 65536)
		for {
			size, from, err := front.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.worker = from
			back := n.back
			n.mu.Unlock()
			_, _ = back.WriteToUDPAddrPort(b[:size], relay)
		}
	})
	t.Cleanup(func() { _ = front.Close(); n.mu.Lock(); _ = n.back.Close(); n.mu.Unlock(); n.done.Wait() })
	return n
}
func (n *udpNAT) rebind(t *testing.T) netip.AddrPort {
	t.Helper()
	back, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	n.mu.Lock()
	old := n.back
	n.back = back
	n.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	n.done.Go(func() {
		b := make([]byte, 65536)
		for {
			size, _, err := back.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			n.mu.Lock()
			worker := n.worker
			n.mu.Unlock()
			if worker.IsValid() {
				_, _ = n.front.WriteToUDPAddrPort(b[:size], worker)
			}
		}
	})
	return back.LocalAddr().(*net.UDPAddr).AddrPort()
}
func TestDialOutNATRebindingDrainsAndRegisters(t *testing.T) {
	forHTTPRecoveryStores(t, testDialOutNATRebinding)
}
func testDialOutNATRebinding(t *testing.T, store sessionstore.Store) {
	var nat *udpNAT
	s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{store: store, dialOut: true, sourceLeg: func(addr netip.AddrPort) netip.AddrPort {
		nat = newUDPNAT(t, addr)
		return nat.front.LocalAddr().(*net.UDPAddr).AddrPort()
	}})
	report := flowingCall(t, s, func(ctx context.Context, call *callharness.Call) {
		before, err := s.plane.Status(ctx)
		require.NoError(t, err)
		old := before.Calls[0].Address
		started := time.Now()
		next := nat.rebind(t)
		// Rebinding by itself cannot change ownership or the relay's route.
		atRebind, err := s.plane.Status(ctx)
		require.NoError(t, err)
		require.Equal(t, old, atRebind.Calls[0].Address)
		require.Eventually(t, func() bool {
			status, err := s.plane.Status(ctx)
			if err != nil || len(status.Calls) != 1 || status.Calls[0].Owner != "1" {
				return false
			}
			for _, w := range status.Workers {
				if w.Name == "0" {
					return w.Address == next && !w.Draining && !w.Dead && w.Link != nil && w.Link.Connected
				}
			}
			return false
		}, 5*time.Second, 20*time.Millisecond)
		moved, err := s.plane.Status(ctx)
		require.NoError(t, err)
		require.Empty(t, moved.Takeovers)
		require.Zero(t, moved.LostCount)
		require.EqualValues(t, 1, moved.Calls[0].MoveCount)
		move, err := s.plane.Move(ctx, call.SessionID(), "0")
		require.NoError(t, err)
		require.Equal(t, "move", move.Kind)
		t.Logf("NAT_REBIND recovery=%s false_takeovers=%d lost_calls=%d", time.Since(started), len(moved.Takeovers), moved.LostCount)
	})
	require.Zero(t, report.DecryptionFailures.Total())
	for _, worker := range s.workers {
		reply := httptest.NewRecorder()
		worker.PrivateHandler().ServeHTTP(reply, httptest.NewRequest("GET", "/metrics", nil))
		require.Contains(t, reply.Body.String(), "relais_worker_decryption_failures_total 0\n")
	}
	t.Log("NAT_REBIND caller_decryption_failures=0 worker_decryption_failures=0")
}
