package mediaworker

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestWorkerStatusObservesInstalledTenuresAndDrain(t *testing.T) {
	address := netip.MustParseAddrPort("127.0.0.1:23456")
	worker := &Worker{sessions: map[string]*session{}}
	// The accessor is independent of transport, agent state, and the store.
	for _, id := range []string{"live", "fenced"} {
		worker.sessions[id] = &session{id: id, lease: sessionstore.Lease{SessionID: id, Worker: address, Epoch: 1<<54 + 7}}
	}
	worker.sessions["fenced"].fenced.Store(true)
	worker.BeginDrain()
	draining, inventory, truncated := worker.installedSessions()
	require.True(t, draining)
	require.False(t, truncated)
	require.Equal(t, []InstalledSession{{ID: "live", Owner: address, Epoch: 1<<54 + 7}}, inventory)
	// Marshal/unmarshal preserves uint64 tenure precision and keeps the existing
	// session count meaning (all registered sessions, even a fencing race).
	response := httptest.NewRecorder()
	worker.PrivateHandler().ServeHTTP(response, httptest.NewRequest("GET", "/status", nil))
	require.Equal(t, 200, response.Code)
	var status WorkerStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.True(t, status.InventoryKnown)
	require.True(t, status.Draining)
	require.Equal(t, 2, status.Sessions)
	require.Equal(t, inventory, status.Inventory)
}
func TestWorkerStatusInventoryIsBounded(t *testing.T) {
	worker := &Worker{sessions: map[string]*session{}}
	for i := range SessionInventoryLimit + 1 {
		id := fmt.Sprint(i)
		worker.sessions[id] = &session{id: id}
	}
	_, inventory, truncated := worker.installedSessions()
	require.True(t, truncated)
	require.Len(t, inventory, SessionInventoryLimit)
}
