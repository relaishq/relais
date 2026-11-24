package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestCrashRunAdmitsRelaySetBeforeCalls(t *testing.T) {
	store := sessionstore.NewMemory()
	var bindings []controlplane.RelayBinding
	var first *relay.Relay
	for _, id := range []string{"r1", "r2"} {
		lease, err := store.ClaimRelay(t.Context(), id, sessionstore.RelayProcess{Owner: id, PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}, time.Hour)
		require.NoError(t, err)
		_, err = store.ActivateRelay(t.Context(), lease)
		require.NoError(t, err)
		r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store, SlotID: id, InstanceID: id, Continuity: &relay.ContinuityStatus{Epoch: lease.Epoch}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		if first == nil {
			first = r
		}
		server := httptest.NewServer(r.PrivateHandler())
		t.Cleanup(server.Close)
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id}, Relay: &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}})
	}
	p := controlplane.New(bindings[0].Relay, store)
	worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: first.WorkerAddr(), PublicAddr: first.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	require.NoError(t, p.Register("w", worker.LocalAddr(), worker))
	require.NoError(t, configureCrashRelaySet(t.Context(), p, bindings))
	status, err := p.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Ready)
	require.Len(t, status.Relays, 2)
	for _, r := range status.Relays {
		require.False(t, r.Pending)
	}
}
