package callharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ADR 0016 section 5: a caller requesting A needs both capability echoes. A
// control that echoes only relay change (a commit-only or pre-amendment
// control) gets the call deleted and relay_unsupported reported.
func TestCallerRequestingADeletesCallWithoutEcho(t *testing.T) {
	var deleted atomic.Bool
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Store(true)
			return
		}
		require.Equal(t, "true", r.Header.Get("Relais-Relay-Change"))
		require.Equal(t, "true", r.Header.Get("Relais-Media-Progress"))
		w.Header().Set("Location", "/calls/old")
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Relais-Relay-Change", "true")
		w.WriteHeader(http.StatusCreated)
	}))
	defer control.Close()
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:9")}})
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = h.Dial(ctx, CallOptions{RelayChange: true, MediaProgress: true})
	require.ErrorIs(t, err, ErrRelayUnsupported)
	require.True(t, deleted.Load(), "the call is deleted, not kept as an unconfirmable relay call")
}
