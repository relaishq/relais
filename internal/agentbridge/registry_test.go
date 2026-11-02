package agentbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func TestBridgeWorkerAcceptsRunnerRegistry(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	_, ok := any(b).(http.Handler)
	require.True(t, ok, "worker must accept inbound registry and token-authenticated bot sockets")
}

func TestBridgeRegistryRejectsCredentialsAndOrigin(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	server := httptest.NewServer(b)
	defer server.Close()
	for _, tc := range []struct {
		token, origin string
		status        int
	}{{"", "", 401}, {"wrong", "", 401}, {"isolated-test-token", "http://localhost", 403}} {
		req, err := http.NewRequest("GET", server.URL+"/bridge/registry", nil)
		require.NoError(t, err)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		req.Header.Set("Origin", tc.origin)
		reply, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, tc.status, reply.StatusCode)
		require.NoError(t, reply.Body.Close())
	}
}

func TestBridgeAttachTokenIsScopedExpiringAndSingleUse(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	server := httptest.NewServer(b)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/bridge"
	scope := mediaworker.BridgeOutputScope{SessionID: "call", BotInstance: "bot", Epoch: 2, Generation: 3, LineageID: "lineage"}
	fixture := func(expires time.Time) (*assignment, attachRequest) {
		p := &assignment{scope: scope, expires: expires, attached: make(chan attachment, 1), claimed: make(chan struct{})}
		token := nonce()
		b.mu.Lock()
		b.pending[token] = p
		b.mu.Unlock()
		return p, attachRequest{Type: "attach", Protocol: 1, SessionID: "call", Bot: "bot", Epoch: 2, Generation: 3, Token: token}
	}
	pending := func(token string) *assignment { b.mu.Lock(); defer b.mu.Unlock(); return b.pending[token] }
	attempt := func(t *testing.T, p *assignment, m attachRequest, accept bool) {
		t.Helper()
		c, err := dialBot(context.Background(), endpoint)
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
		start := time.Now()
		require.NoError(t, websocket.JSON.Send(c, m))
		if accept {
			select {
			case a := <-p.attached:
				close(p.claimed)
				close(a.done)
			case <-time.After(time.Second):
				t.Fatal("valid attachment was not delivered")
			}
		}
		var reply []byte
		require.Error(t, websocket.Message.Receive(c, &reply))
		require.Empty(t, p.attached, "refused attempts must never enqueue an attachment")
		require.Less(t, time.Since(start), 200*time.Millisecond, "refusal must close promptly, not expire an accepted attachment")
	}
	for _, tc := range []struct {
		name   string
		change func(*attachRequest)
	}{
		{"session", func(m *attachRequest) { m.SessionID = "other" }},
		{"epoch", func(m *attachRequest) { m.Epoch++ }},
		{"generation", func(m *attachRequest) { m.Generation++ }},
		{"bot", func(m *attachRequest) { m.Bot = "other" }},
		{"unknown-token", func(m *attachRequest) { m.Token = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, m := fixture(time.Now().Add(5 * time.Second))
			token := m.Token
			tc.change(&m)
			attempt(t, p, m, false)
			require.Same(t, p, pending(token), "scope mismatch cannot consume a valid token")
		})
	}
	t.Run("single-use", func(t *testing.T) {
		p, m := fixture(time.Now().Add(5 * time.Second))
		attempt(t, p, m, true)
		require.Nil(t, pending(m.Token), "valid use must consume the token")
		attempt(t, p, m, false)
		require.Nil(t, pending(m.Token))
	})
	t.Run("expiry", func(t *testing.T) { p, m := fixture(time.Now().Add(-time.Second)); attempt(t, p, m, false) })
}

func TestBridgeReservationDiscardsLateReplyAfterCancellation(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	server := httptest.NewServer(b)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialBot(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge/registry")
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, websocket.JSON.Send(c, map[string]any{"type": "hello", "protocol": 1, "runner_id": "runner", "bot_instance": "bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var registered struct {
		Type string `json:"type"`
	}
	require.NoError(t, websocket.JSON.Receive(c, &registered))
	require.Equal(t, "registered", registered.Type)
	b.mu.Lock()
	r := b.runner
	b.mu.Unlock()
	type result struct {
		runner     *runner
		assignment *assignment
		err        error
	}
	start := func(ctx context.Context) <-chan result {
		done := make(chan result, 1)
		go func() { r, p, e := b.reserve(ctx, "call", 1, mediaworker.BridgeOutputScope{}); done <- result{r, p, e} }()
		return done
	}
	var request struct {
		Type string `json:"type"`
		ID   string `json:"reservation_id"`
	}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	first := start(firstCtx)
	require.NoError(t, websocket.JSON.Receive(c, &request))
	require.Equal(t, "reserve", request.Type)
	cancelFirst()
	require.ErrorIs(t, (<-first).err, context.Canceled)
	// Deterministically model register() publishing the delayed old response
	// after the canceled request released its slot. No sleeps or queue polling.
	r.replies <- reservationReply{Type: "reserved", ID: request.ID, Accepted: true}
	for i := 0; i < 3; i++ {
		done := start(ctx)
		require.NoError(t, websocket.JSON.Receive(c, &request))
		require.Equal(t, "reserve", request.Type)
		require.NoError(t, websocket.JSON.Send(c, reservationReply{Type: "reserved", ID: request.ID, Accepted: true}))
		next := <-done
		require.NoError(t, next.err, "later reservation %d must survive the stale reply", i+1)
		var assigned struct {
			Type string `json:"type"`
		}
		require.NoError(t, websocket.JSON.Receive(c, &assigned))
		require.Equal(t, "assign", assigned.Type)
		b.removeAssignment(next.assignment)
		<-next.runner.slot
	}
}
