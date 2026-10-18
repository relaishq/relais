package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateErrorsKeepMainPublicResponse(t *testing.T) {
	for _, status := range []int{400, 409, 500, 503} {
		for _, code := range []string{"worker_draining", "no_target", "unknown"} {
			server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "retryable": true, "message": "private detail"})
			}))
			resp, body := request(t, client, http.MethodPost, server.URL+"/calls", "offer", testToken)
			require.Equal(t, status, resp.StatusCode)
			require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
			require.Equal(t, "signaling request failed; operation outcome may be uncertain\n", body)
			require.Empty(t, resp.Header.Get("Retry-After"))
		}
	}
}

func TestCreateCapacityRefusalIsRetryable(t *testing.T) {
	server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "999")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "at_capacity", "message": "private detail"})
	}))
	resp, body := request(t, client, http.MethodPost, server.URL+"/calls", "offer", testToken)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var failure map[string]string
	require.NoError(t, json.Unmarshal([]byte(body), &failure))
	require.Equal(t, "at_capacity", failure["code"])
	require.NotContains(t, body, "private detail")
	require.Equal(t, "retryable", controlErrors["at_capacity"].class)
}
