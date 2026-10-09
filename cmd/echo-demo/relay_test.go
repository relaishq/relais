package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/pion/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/mediaworker"
)

// TestRelayMode checks the -relay media side: a relay on the media address
// with the worker on a private socket behind it, and the call endpoints the
// page uses. Calls through the relay are covered by the call harness.
func TestRelayMode(t *testing.T) {
	sys, err := startMedia(netip.MustParseAddrPort("127.0.0.1:0"), true, logging.NewDefaultLoggerFactory())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sys.close()) })

	r, ok := sys.(*relayed)
	require.True(t, ok, "relay mode runs the relayed media side, not %T", sys)
	assert.Equal(t, r.relay.PublicAddr(), r.worker.MediaAddr(), "answers advertise the relay")
	assert.NotEqual(t, r.relay.PublicAddr(), r.worker.LocalAddr(), "the worker has its own socket")
	assert.True(t, r.worker.LocalAddr().Addr().IsLoopback(), "the worker's socket is private")

	server := httptest.NewServer(newHandler(sys))
	t.Cleanup(server.Close)
	post := func(path, contentType, body string) (int, string) {
		resp, err := http.Post(server.URL+path, contentType, strings.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return resp.StatusCode, string(data)
	}

	status, body := post(mediaworker.CallsPath, "application/sdp", "not an offer")
	assert.Equal(t, http.StatusBadRequest, status, "POST /calls with a malformed offer")
	assert.Contains(t, body, "unsupported offer")

	status, body = post(mediaworker.CallsPath+"/somecall/move", "", "")
	assert.Equal(t, http.StatusNotImplemented, status, "POST /calls/{id}/move through the relay")
	assert.Contains(t, body, "not supported yet")
}

// TestDefaultModeSharesASocket checks that without -relay the demo runs #4's
// two workers on one shared socket.
func TestDefaultModeSharesASocket(t *testing.T) {
	sys, err := startMedia(netip.MustParseAddrPort("127.0.0.1:0"), false, logging.NewDefaultLoggerFactory())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sys.close()) })

	s, ok := sys.(*system)
	require.True(t, ok, "default mode runs the shared-socket system, not %T", sys)
	require.Len(t, s.workers, 2)
	for _, w := range s.workers {
		assert.Equal(t, s.socket.LocalAddr(), w.worker.MediaAddr(), "worker %s advertises the shared socket", w.name)
	}
}
