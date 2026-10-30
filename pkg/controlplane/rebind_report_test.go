package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Relay API boundary: each admission's token binds once to the observed source.
type bindingRelayHandler struct {
	mu       sync.Mutex
	observed netip.AddrPort
	bound    map[string]netip.AddrPort
	workers  map[netip.AddrPort]bool
}

func (h *bindingRelayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var req relay.WorkerRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	switch {
	case r.URL.Path == "/workers/expect":
		h.bound[req.Token] = h.observed
	case r.URL.Path == "/workers" && r.Method == http.MethodPost:
		if h.bound[req.Token] != req.Address {
			http.Error(w, "token bound to another address", http.StatusConflict)
			return
		}
		h.workers[req.Address] = true
	case r.URL.Path == "/workers" && r.Method == http.MethodDelete:
		delete(h.workers, req.Address)
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *bindingRelayHandler) observe(addr netip.AddrPort) {
	h.mu.Lock()
	h.observed = addr
	h.mu.Unlock()
}

func TestRebindFlapBackAfterAdmissionAtNewAddress(t *testing.T) {
	original := netip.MustParseAddrPort("127.0.0.1:17001")
	next := netip.MustParseAddrPort("127.0.0.1:17002")
	h := &bindingRelayHandler{observed: original, bound: make(map[string]netip.AddrPort), workers: make(map[netip.AddrPort]bool)}
	r := newPipeAdmissionRig(t, h, sessionstore.NewMemory())
	authorizeLongControl(t, r.p)
	stream, err := r.open(t, "A", 1, 0, original)
	require.NoError(t, err)
	first := stream.run(statusHandler())
	// The authoritative call still belongs to O when the link reconnects at N.
	lease, err := r.p.store.Claim(context.Background(), "call", original, time.Minute)
	require.NoError(t, err)
	h.observe(next)
	require.NoError(t, reportLeg(context.Background(), &http.Client{Transport: first}, next))
	status := mediaworker.WorkerStatus{InventoryKnown: true, Sessions: 1, Inventory: []mediaworker.InstalledSession{{ID: "call", Owner: original, Epoch: lease.Epoch}}}
	stream, err = r.openWorkerWithStatus(t, "a", "A", 2, 0, next, status)
	require.NoError(t, err)
	require.Equal(t, original, stream.ready.Address)
	peer := stream.run(statusHandler())
	h.observe(original)
	_ = reportLeg(context.Background(), &http.Client{Transport: peer}, original)
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("flap-back left a live link whose token can never bind the original address")
	}
	stream, err = r.open(t, "A", 3, 0, original)
	require.NoError(t, err)
	stream.run(statusHandler())
	state, err := r.p.Status(context.Background())
	require.NoError(t, err)
	require.False(t, state.Workers[0].Draining, "automatic drain must end after fresh original-address admission")
	r.p.mu.Lock()
	target := r.p.workers["a"].rebindTarget
	r.p.mu.Unlock()
	require.False(t, target.IsValid(), "fresh original-address admission must clear automatic drain")
	lease, err = r.p.store.Get(context.Background(), "call")
	require.NoError(t, err)
	require.Equal(t, original, lease.Worker)
	require.Zero(t, state.LostCount)
}

func TestReplacementRemovesPreAddressRebindRegistration(t *testing.T) {
	original := netip.MustParseAddrPort("127.0.0.1:17001")
	next := netip.MustParseAddrPort("127.0.0.1:17002")
	h := &bindingRelayHandler{observed: original, bound: make(map[string]netip.AddrPort), workers: make(map[netip.AddrPort]bool)}
	r := newPipeAdmissionRig(t, h, sessionstore.NewMemory())
	authorizeLongControl(t, r.p)
	stream, err := r.open(t, "A", 1, 0, original)
	require.NoError(t, err)
	stream.run(statusHandler())
	// Live calls force beginRebind rather than readdress during admission.
	lease, err := r.p.store.Claim(context.Background(), "call", original, time.Minute)
	require.NoError(t, err)
	h.observe(next)
	stream, err = r.open(t, "A", 2, 0, next)
	require.NoError(t, err)
	stream.run(statusHandler())
	require.NoError(t, r.p.store.Release(context.Background(), lease))
	r.declareDead()
	stream, err = r.open(t, "B", 1, 0, next)
	require.NoError(t, err)
	stream.run(statusHandler())
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, map[netip.AddrPort]bool{next: true}, h.workers, "replacement left the abandoned original address registered")
}
