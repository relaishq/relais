package relay

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestPrivateMoveRequiresExplicitRepair(t *testing.T) {
	r, err := New(Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: sessionstore.NewMemory()})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.AddWorker(netip.MustParseAddrPort("127.0.0.1:19000"))
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"to":"127.0.0.1:19000"}`, http.StatusBadRequest},
		{`{"from":"","to":"127.0.0.1:19000"}`, http.StatusBadRequest},
		{`{"repair":true,"to":"127.0.0.1:19000"}`, http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodPost, "/sessions/test/move", strings.NewReader(tc.body))
		response := httptest.NewRecorder()
		r.PrivateHandler().ServeHTTP(response, req)
		require.Equal(t, tc.status, response.Code, response.Body.String())
	}
}
