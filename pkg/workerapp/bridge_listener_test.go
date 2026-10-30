package workerapp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func TestDevelopmentBridgeListenerServesRegistry(t *testing.T) {
	addr := redistestenv.Address(t)
	if addr == "" {
		t.Skip("set RELAIS_TEST_REDIS_ADDR to dedicated Redis")
	}
	clearWorkerTopologyEnvironment(t)
	t.Setenv("RELAIS_SESSIONSTORE_KEY", strings.Repeat("00", 32))
	t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", "")
	t.Setenv("RELAIS_SESSIONSTORE_OLD_KEYS", "")
	t.Setenv("RELAIS_REDIS_USERNAME", "")
	t.Setenv("RELAIS_REDIS_PASSWORD", "")
	t.Setenv("RELAIS_REDIS_CLUSTER", "0")
	t.Setenv("RELAIS_BRIDGE_DEV_TOKEN", "isolated-test-token")
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer control.Close()
	available, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bridgeURL := "ws://" + available.Addr().String() + "/bridge"
	require.NoError(t, available.Close())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"-name=bridge-listener-test", "-agent=bridge", "-agent-split-checkpoints", "-bridge-url=" + bridgeURL, "-redis=" + addr, "-redis-prefix=bridge-listener-test:", "-control=" + control.URL, "-relay-leg=127.0.0.1:2", "-relay-media=127.0.0.1:3", "-frame-cache-off"}, Options{})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	cfg, err := websocket.NewConfig(bridgeURL+"/registry", "http://localhost")
	require.NoError(t, err)
	cfg.Origin = &url.URL{}
	cfg.Header.Set("Authorization", "Bearer isolated-test-token")
	var conn *websocket.Conn
	require.Eventually(t, func() bool {
		dialCtx, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		conn, err = cfg.DialContext(dialCtx)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, websocket.JSON.Send(conn, map[string]any{"type": "hello", "protocol": 1, "runner_id": "listener-runner", "bot_instance": "listener-bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var reply struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}
	require.NoError(t, websocket.JSON.Receive(conn, &reply))
	require.Equal(t, "registered", reply.Type)
	require.Equal(t, 1, reply.Protocol)
}
