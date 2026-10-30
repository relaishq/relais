package agentbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func TestBridgeRegistryLossSettlesAttachedCall(t *testing.T) {
	for _, fault := range []string{"close", "invalid-message"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			b, endpoint, id := authorityCall(t, ctx)
			registry, err := dialBot(ctx, endpoint+"/registry")
			require.NoError(t, err)
			defer func() { _ = registry.Close() }()
			require.NoError(t, registry.SetDeadline(time.Now().Add(10*time.Second)))
			require.NoError(t, websocket.JSON.Send(registry, map[string]any{"type": "hello", "protocol": 1, "runner_id": "runner", "bot_instance": "bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
			var message map[string]json.RawMessage
			require.NoError(t, websocket.JSON.Receive(registry, &message))
			require.NoError(t, websocket.JSON.Receive(registry, &message))
			require.NoError(t, websocket.JSON.Send(registry, map[string]any{"type": "reserved", "reservation_id": message["reservation_id"], "accepted": true}))
			require.NoError(t, websocket.JSON.Receive(registry, &message))
			bot, err := dialBot(ctx, endpoint)
			require.NoError(t, err)
			defer func() { _ = bot.Close() }()
			require.NoError(t, attachFake(bot, message, 0))
			require.NoError(t, websocket.JSON.Receive(bot, &message))
			outcome := func() string {
				response := httptest.NewRecorder()
				b.worker.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status?agent-session="+id, nil))
				var status mediaworker.WorkerStatus
				if json.Unmarshal(response.Body.Bytes(), &status) != nil || status.AgentSession == nil {
					return ""
				}
				var app map[string]any
				_ = json.Unmarshal(status.AgentSession.App, &app)
				value, _ := app["bot_outcome"].(string)
				return value
			}
			require.Eventually(t, func() bool { return outcome() == "attached" }, 3*time.Second, 10*time.Millisecond)
			started := time.Now()
			if fault == "close" {
				require.NoError(t, registry.Close())
			} else {
				require.NoError(t, websocket.JSON.Send(registry, map[string]any{"type": "invalid"}))
			}
			require.Eventually(t, func() bool { return outcome() == "no_capacity_silence" }, 4*time.Second, 10*time.Millisecond)
			require.Equal(t, 1, b.worker.SessionCount())
			b.mu.Lock()
			require.Empty(t, b.active)
			b.mu.Unlock()
			t.Logf("fault=%s settlement=%s caller_retained=true", fault, time.Since(started))
		})
	}
}
