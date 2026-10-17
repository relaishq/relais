package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unknown-conflict check: refusal codes must survive
// the public boundary, and unfamiliar replies must never imply invalid input.
func TestControlErrorOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		body   string
		want   string
	}{
		{code: "agent_backpressure", status: 409, want: "agent_backpressure"},
		{code: "move_budget", status: 409, want: "move_budget"},
		{code: "agent_incompatible", status: 409, want: "agent_incompatible"},
		{code: "closed", status: 409, want: "closed"},
		{code: "internal", status: 409, want: "uncertain"},
		{code: "future_code", status: 400, want: "uncertain"},
		{status: 409, body: `{}`, want: "uncertain"},
		{status: 400, body: `{"message":"unknown failure"}`, want: "uncertain"},
		{status: 400, body: `invalid JSON request`, want: "control_invalid"},
		{status: 502, body: `invalid response`, want: "uncertain"},
	} {
		t.Run(tc.code+tc.body, func(t *testing.T) {
			server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "message": "private detail"})
			}))
			resp, body := request(t, client, http.MethodPost, server.URL+"/calls/id/agent/control", `{"id":"a","sequence":1,"name":"stop"}`, testToken)
			require.Equal(t, tc.status, resp.StatusCode)
			var failure struct{ Code string }
			require.NoError(t, json.Unmarshal([]byte(body), &failure))
			require.Equal(t, tc.want, failure.Code)
			require.NotContains(t, body, "private detail")
		})
	}
}
