package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
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

func TestRelayAdvertisesMappedAddressWithoutBindingIt(t *testing.T) {
	advertised := netip.MustParseAddrPort("192.0.2.10:24780")
	for _, address := range []netip.AddrPort{{}, advertised} {
		r, err := New(Config{Owners: sessionstore.NewMemory(), AdvertiseAddr: address})
		require.NoError(t, err)
		response := httptest.NewRecorder()
		r.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
		var status RelayStatus
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
		want := r.publicAddr
		if address.IsValid() {
			want = address
		}
		require.Equal(t, want, status.Public)
		require.True(t, r.publicAddr.Addr().IsLoopback(), "mapped public address must never be bound locally")
		require.NoError(t, r.Close())
	}
	for _, address := range []string{"0.0.0.0:24780", "192.0.2.10:0", "224.0.0.1:24780"} {
		_, err := New(Config{Owners: sessionstore.NewMemory(), AdvertiseAddr: netip.MustParseAddrPort(address)})
		require.ErrorContains(t, err, "invalid advertised media address")
	}
}
