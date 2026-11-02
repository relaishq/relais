package controlplane_test

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

type udpNAT struct {
	*nettopology.NATMapping
	original netip.AddrPort
}

func newUDPNAT(t *testing.T, relay netip.AddrPort) *udpNAT {
	t.Helper()
	mapping, err := nettopology.NewNATMapping(relay)
	require.NoError(t, err)
	n := &udpNAT{NATMapping: mapping, original: mapping.ObservedAddr()}
	t.Cleanup(func() { require.NoError(t, n.Close()) })
	return n
}
func (n *udpNAT) rebind(t *testing.T) netip.AddrPort {
	t.Helper()
	require.NoError(t, n.Rebind(netip.AddrPort{}))
	return n.ObservedAddr()
}
func TestDialOutNATRebindingDrainsAndRegisters(t *testing.T) {
	forHTTPRecoveryStores(t, testDialOutNATRebinding)
}
func testDialOutNATRebinding(t *testing.T, store sessionstore.Store) {
	var nat *udpNAT
	s := newHTTPSystemWithSettings(t, nil, nil, nil, httpSystemSettings{store: store, dialOut: true, sourceLeg: func(addr netip.AddrPort) netip.AddrPort {
		nat = newUDPNAT(t, addr)
		return nat.WorkerAddr()
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
