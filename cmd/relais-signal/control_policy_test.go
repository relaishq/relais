package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// Expected classes are independent of the signal whitelist. Enumerate the actual
// emitters, so adding a private code requires a public policy and documentation.
func TestControlErrorPolicyCompleteness(t *testing.T) {
	classes := map[string]string{}
	groups := map[string]string{
		"retryable": "worker_link_unavailable worker_command_not_sent stale_tenure control_not_ready control_rejected agent_backpressure agent_capacity closed not_established unknown_session lease_lost move_in_progress no_target at_capacity transient deadline canceled session_exists checkpoint_clock export_not_flushed barrier_timeout held hold_limit hold_expired replay_in_progress replay_not_ready replay_target_changed",
		"definite":  "control_invalid control_conflict control_order control_stale control_unsupported move_budget agent_incompatible agent_save agent_restore agent_version agent_oversize unknown_call not_found ended unsupported_offer sequence_budget srtcp_exhausted dtls_exhausted export_incompatible buffer_disabled",
		"uncertain": "uncertain internal",
	}
	for class, codes := range groups {
		for _, code := range strings.Fields(codes) {
			classes[code] = class
		}
	}
	emitted := map[string]bool{"internal": true, "uncertain": true}
	for _, codes := range []map[string]error{privateapi.CommonErrors, mediaworker.RemoteErrors, controlplane.RemoteErrors} {
		for code := range codes {
			emitted[code] = true
		}
	}
	// Placement refusals come only from createSession, never Control.
	delete(emitted, "worker_draining")
	require.Len(t, classes, len(emitted), "classification and emitter inventories differ")
	docs := map[string]string{}
	for _, file := range []string{"../../docs/api_reference.md", "../../docs/build-an-agent.md"} {
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		docs[file] = string(body)
	}
	for code := range emitted {
		t.Run(code, func(t *testing.T) {
			class, ok := classes[code]
			require.True(t, ok, "unclassified emitted code %s", code)
			for file, body := range docs {
				var documented bool
				for _, line := range strings.Split(body, "\n") {
					if strings.HasPrefix(line, "| "+class+" |") && strings.Contains(line, "`"+code+"`") {
						documented = true
					}
				}
				require.True(t, documented, "%s must document %s as %s", file, code, class)
			}
			server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(privateapi.Failure{Code: code, Message: "private address"})
			}))
			_, body := request(t, client, http.MethodPost, server.URL+"/calls/id/agent/control", `{"id":"a","sequence":1,"name":"stop"}`, testToken)
			var failure struct{ Code, Message string }
			require.NoError(t, json.Unmarshal([]byte(body), &failure))
			expected := code
			if class == "uncertain" {
				expected = "uncertain"
			}
			require.Equal(t, expected, failure.Code)
			switch class {
			case "retryable":
				require.Contains(t, failure.Message, "retry the identical message")
			case "definite":
				require.NotContains(t, failure.Message, "retry the identical message")
			}
			require.NotContains(t, body, "private address")
		})
	}
}
