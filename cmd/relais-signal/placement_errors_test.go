package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlacementRefusalRemainsTypedAndRetryable(t *testing.T) {
	for _, code := range []string{"worker_draining", "no_target"} {
		t.Run(code, func(t *testing.T) {
			server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(503)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "retryable": true, "message": "private detail"})
			}))
			resp, body := request(t, client, http.MethodPost, server.URL+"/calls", "offer", testToken)
			require.Equal(t, 503, resp.StatusCode)
			var failure struct {
				Code      string
				Retryable bool
			}
			require.NoError(t, json.Unmarshal([]byte(body), &failure))
			require.Equal(t, code, failure.Code)
			require.True(t, failure.Retryable)
			require.NotContains(t, body, "private detail")
			require.Equal(t, "1", resp.Header.Get("Retry-After"))
		})
	}
}
