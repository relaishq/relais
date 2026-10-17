package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestControlFrontendReceiptsAndSafeErrors(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusOK, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			called := 0
			server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called++
				require.Equal(t, "/calls/id/agent/control", r.URL.Path)
				require.Empty(t, r.URL.RawQuery)
				require.Empty(t, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusConflict {
					_, _ = w.Write([]byte(`{"code":"control_conflict","message":"private worker at 10.42.0.9:9000 secret"}`))
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(agent.ControlReceipt{ID: "press", Sequence: 1, EventID: "control/stable", Checkpointed: status == http.StatusOK}))
			}))
			resp, body := request(t, client, http.MethodPost, server.URL+"/calls/id/agent/control", `{"id":"press","sequence":1,"name":"ptt.start"}`, testToken)
			require.Equal(t, status, resp.StatusCode)
			require.Equal(t, 1, called)
			if status == http.StatusConflict {
				require.Contains(t, body, "control_conflict")
				require.NotContains(t, body, "10.42.0.9")
				require.NotContains(t, body, "secret")
			}
			resp, _ = request(t, client, http.MethodPost, server.URL+"/calls/id/agent/control?worker=1", `{}`, testToken)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Equal(t, 1, called)
			resp, _ = request(t, client, http.MethodPost, server.URL+"/calls/id/agent/control", `{}`, "")
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.Equal(t, 1, called)
		})
	}
	for _, path := range []string{"/calls/id/move/agent/control", "/calls/id/../agent/control", "/calls/%69d/agent/control", "/calls/id/agent/control/"} {
		require.False(t, controlPath(path) || strings.HasSuffix(path, "%69d"))
	}
}
