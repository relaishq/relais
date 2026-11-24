package callharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func newHarnessRelay(t *testing.T, store sessionstore.Store, id string) *relay.Relay {
	t.Helper()
	leases := store.(sessionstore.RelayLeases)
	lease, err := leases.ClaimRelay(t.Context(), id, sessionstore.RelayProcess{Owner: id, PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, leases.ReleaseRelay(context.Background(), lease)) })
	_, err = leases.ActivateRelay(t.Context(), lease)
	require.NoError(t, err)
	r, err := relay.New(relay.Config{Owners: store, SlotID: id, InstanceID: id, Continuity: &relay.ContinuityStatus{Epoch: lease.Epoch}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

// Use the same configure-and-sync admission path as crash-run. The status
// wrapper retains the fault proxy's advertised public address in rollback tests.
func configureHarnessRelaySet(t *testing.T, ctx context.Context, p *controlplane.Plane, bindings []controlplane.RelayBinding) {
	t.Helper()
	require.NoError(t, p.ConfigureRelaySet(ctx, bindings))
	for _, b := range bindings {
		r := b.Relay.(*relay.Relay)
		handler := r.PrivateHandler()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/status" {
				reply := httptest.NewRecorder()
				handler.ServeHTTP(reply, req)
				var status relay.RelayStatus
				require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &status))
				status.Public = b.Slot.Public
				privateapi.Write(w, status)
				return
			}
			handler.ServeHTTP(w, req)
		}))
		t.Cleanup(server.Close)
		remote := &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}
		registry := controlplane.RelayWorkerRegistry{Slot: b.Slot.ID}
		require.NoError(t, registry.Sync(ctx, p, remote))
	}
}
