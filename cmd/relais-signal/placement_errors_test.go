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
