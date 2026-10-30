package agentbridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func TestBridgeRevocationRetriesAndRejectsWrongAcknowledgements(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	server := httptest.NewServer(b)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialBot(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge/registry")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, websocket.JSON.Send(conn, map[string]any{"type": "hello", "protocol": 1, "runner_id": "runner", "bot_instance": "bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var registered map[string]any
	require.NoError(t, websocket.JSON.Receive(conn, &registered))
	b.mu.Lock()
	r := b.runner
	b.mu.Unlock()
	for _, newEpoch := range []uint64{1, 2} {
		request := revocation{SessionID: "call", Bot: "bot", OldEpoch: 1, OldGeneration: 10, NewEpoch: newEpoch, NewGeneration: 11, RequestID: nonce()}
		done := make(chan error, 1)
		go func() { done <- r.revoke(ctx, request) }()
		var first revocation
		require.NoError(t, websocket.JSON.Receive(conn, &first))
		require.Equal(t, "revoke", first.Type)
		wrong := first
		wrong.Type = "revoked"
		wrong.NewGeneration++
		require.NoError(t, websocket.JSON.Send(conn, wrong))
		// The next attempt, rather than an elapsed sleep, proves a mismatch did not authorize.
		var retry revocation
		require.NoError(t, websocket.JSON.Receive(conn, &retry))
		require.Equal(t, first, retry, "all arguments and request ID must survive retries")
		wrong = first
		wrong.Type = "revoked"
		wrong.RequestID = "stale"
		require.NoError(t, websocket.JSON.Send(conn, wrong))
		require.NoError(t, websocket.JSON.Receive(conn, &retry))
		require.Equal(t, first, retry)
		ack := first
		ack.Type = "revoked"
		require.NoError(t, websocket.JSON.Send(conn, ack))
		require.NoError(t, <-done)
		require.NoError(t, websocket.JSON.Send(conn, ack))
		r.replyMu.Lock()
		require.Nil(t, r.revoking)
		r.replyMu.Unlock()
	}
}

func TestBridgeRevokedAcknowledgementMatchesEveryField(t *testing.T) {
	request := revocation{Type: "revoke", SessionID: "call", Bot: "bot", OldEpoch: 1, OldGeneration: 2, NewEpoch: 2, NewGeneration: 3, RequestID: "request"}
	r := &runner{revoking: &request, revoked: make(chan struct{}, 1)}
	for _, change := range []func(*revocation){
		func(v *revocation) { v.SessionID = "other" }, func(v *revocation) { v.Bot = "other" },
		func(v *revocation) { v.OldEpoch++ }, func(v *revocation) { v.OldGeneration++ },
		func(v *revocation) { v.NewEpoch++ }, func(v *revocation) { v.NewGeneration++ },
		func(v *revocation) { v.RequestID = "stale" },
	} {
		ack := request
		ack.Type = "revoked"
		change(&ack)
		r.receiveRevoked(ack)
		require.Empty(t, r.revoked)
	}
	ack := request
	ack.Type = "revoked"
	r.receiveRevoked(ack)
	r.receiveRevoked(ack)
	require.Len(t, r.revoked, 1, "duplicate acknowledgement cannot be queued twice")
	<-r.revoked
	r.revoking = nil
	r.receiveRevoked(ack)
	require.Empty(t, r.revoked)
}

func TestBridgeStatusMetadataFitsWorkingAllowance(t *testing.T) {
	a := newAdapter(t)
	raw, err := json.Marshal(checkpoint{Sequence: 1, Version: 1, Working: []byte(strings.Repeat("x", 500))})
	require.NoError(t, err)
	// This fit the old 128-byte reserve, which cannot hold a maximum runner ID,
	// the new bounded bot outcome and a maximum-width outbox revision together.
	a.maxWorking = len(raw) + 128
	require.ErrorIs(t, a.candidate(raw), agent.ErrCapacity)
}

func TestBridgeRestartedRunnerCannotBorrowLiveSessionPermit(t *testing.T) {
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	b.active["call"] = activeBot{scope: mediaworker.BridgeOutputScope{SessionID: "call", BotInstance: "bot", Epoch: 1, Generation: 1}, done: make(chan struct{})}
	server := httptest.NewServer(b)
	defer server.Close()
	c, err := dialBot(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge/registry")
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, websocket.JSON.Send(c, map[string]any{"type": "hello", "protocol": 1, "runner_id": "restarted", "bot_instance": "bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var registered map[string]any
	require.NoError(t, websocket.JSON.Receive(c, &registered))
	require.NoError(t, websocket.JSON.Send(c, permitRequest{Type: "permit_request", SessionID: "call", Epoch: 1, Generation: 1, Nonce: "request"}))
	var reply struct {
		Remaining int64 `json:"remaining_ms"`
	}
	require.NoError(t, websocket.JSON.Receive(c, &reply))
	require.Zero(t, reply.Remaining)
}
