package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/mediaworker"
)

// TestHandlerServesPageAndSignaling checks the demo server's routes: the
// embedded page and script, and the worker's signaling endpoint on the same
// origin. Calls themselves are covered by the call harness.
func TestHandlerServesPageAndSignaling(t *testing.T) {
	worker, err := mediaworker.New(mediaworker.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, worker.Close()) })

	server := httptest.NewServer(newHandler(worker))
	t.Cleanup(server.Close)

	get := func(path string) (int, string) {
		resp, err := http.Get(server.URL + path)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return resp.StatusCode, string(body)
	}

	status, page := get("/")
	assert.Equal(t, http.StatusOK, status, "GET /")
	assert.Contains(t, page, `id="stats"`)
	assert.Contains(t, page, `src="demo.js"`)

	status, script := get("/demo.js")
	assert.Equal(t, http.StatusOK, status, "GET /demo.js")
	assert.Contains(t, script, "window.relaisStats")

	// The signaling endpoint answers, so it is the worker's handler and not
	// the file server: a malformed offer is rejected as unsupported.
	resp, err := http.Post(server.URL+mediaworker.CallsPath, "application/sdp", strings.NewReader("not an offer"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "POST /calls with a malformed offer")
	assert.Contains(t, string(body), "unsupported offer")
}
