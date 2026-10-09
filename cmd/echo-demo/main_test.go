package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/mediaworker"
)

// TestHandlerServesPageAndSignaling checks the demo server's routes: the
// embedded page and scripts, the signaling endpoint and the call endpoints
// on the same origin. Calls and moves themselves are covered by the call
// harness.
func TestHandlerServesPageAndSignaling(t *testing.T) {
	sys, err := startSystem("127.0.0.1:0", logging.NewDefaultLoggerFactory())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sys.close()) })

	server := httptest.NewServer(newHandler(sys))
	t.Cleanup(server.Close)

	do := func(method, path, contentType, body string) (int, string) {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return resp.StatusCode, string(data)
	}

	status, page := do(http.MethodGet, "/", "", "")
	assert.Equal(t, http.StatusOK, status, "GET /")
	assert.Contains(t, page, `id="stats"`)
	assert.Contains(t, page, `src="demo.js"`)
	assert.Contains(t, page, `src="handover.js"`)
	assert.Contains(t, page, `id="move"`)

	status, script := do(http.MethodGet, "/demo.js", "", "")
	assert.Equal(t, http.StatusOK, status, "GET /demo.js")
	assert.Contains(t, script, "window.relaisStats")

	status, script = do(http.MethodGet, "/handover.js", "", "")
	assert.Equal(t, http.StatusOK, status, "GET /handover.js")
	assert.Contains(t, script, "window.relaisMove")

	// The signaling endpoint answers, so it is the workers' handler and not
	// the file server: a malformed offer is rejected as unsupported.
	status, body := do(http.MethodPost, mediaworker.CallsPath, "application/sdp", "not an offer")
	assert.Equal(t, http.StatusBadRequest, status, "POST /calls with a malformed offer")
	assert.Contains(t, body, "unsupported offer")

	// The call endpoints exist and know no such call.
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, mediaworker.CallsPath + "/nosuchcall/move"},
		{http.MethodGet, mediaworker.CallsPath + "/nosuchcall"},
		{http.MethodDelete, mediaworker.CallsPath + "/nosuchcall"},
	} {
		status, _ := do(req.method, req.path, "", "")
		assert.Equal(t, http.StatusNotFound, status, "%s %s", req.method, req.path)
	}
}
