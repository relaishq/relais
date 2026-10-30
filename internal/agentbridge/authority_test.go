package agentbridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func authorityCall(t *testing.T, ctx context.Context) (*Bridge, string, string) {
	t.Helper()
	b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
	require.NoError(t, err)
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	store := sessionstore.NewMemory()
	rel, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rel.Close()) })
	factory := agenthost.Factory(b.Registration())
	w, err := mediaworker.New(mediaworker.Config{SplitCheckpoints: true, Agent: mediaworker.AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"bridge": factory}, CallbackTimeout: time.Second}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: rel.WorkerAddr(), PublicAddr: rel.PublicAddr(), LeaseRenewInterval: 100 * time.Millisecond}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	b.SetWorker(w)
	rel.AddWorker(w.LocalAddr())
	var setting webrtc.SettingEngine
	setting.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(setting)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pc.Close()) })
	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	require.NoError(t, err)
	gather := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	require.NoError(t, pc.SetLocalDescription(offer))
	<-gather
	id, answer, err := w.CreateSession(ctx, pc.LocalDescription().SDP)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.EndSession(id)) })
	require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))
	return b, "ws" + strings.TrimPrefix(server.URL, "http") + "/bridge", id
}

func TestBridgePermitRepliesAndPartitionExpiryFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	b, endpoint, id := authorityCall(t, ctx)
	registry, err := dialBot(ctx, endpoint+"/registry")
	require.NoError(t, err)
	defer func() { _ = registry.Close() }()
	require.NoError(t, registry.SetDeadline(time.Now().Add(12*time.Second)))
	require.NoError(t, websocket.JSON.Send(registry, map[string]any{"type": "hello", "protocol": 1, "runner_id": "runner", "bot_instance": "bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}))
	var registered map[string]any
	require.NoError(t, websocket.JSON.Receive(registry, &registered))
	var reserve map[string]json.RawMessage
	require.NoError(t, websocket.JSON.Receive(registry, &reserve))
	require.NoError(t, websocket.JSON.Send(registry, map[string]any{"type": "reserved", "reservation_id": reserve["reservation_id"], "accepted": true}))
	var assign map[string]json.RawMessage
	require.NoError(t, websocket.JSON.Receive(registry, &assign))
	bot, err := dialBot(ctx, endpoint)
	require.NoError(t, err)
	defer func() { _ = bot.Close() }()
	require.NoError(t, attachFake(bot, assign, 0))
	var attached struct {
		Scope mediaworker.BridgeOutputScope `json:"scope"`
	}
	require.NoError(t, websocket.JSON.Receive(bot, &attached))
	botDone := make(chan struct{})
	go func() {
		defer close(botDone)
		for {
			var raw []byte
			if websocket.Message.Receive(bot, &raw) != nil {
				return
			}
		}
	}()
	defer func() { _ = bot.Close(); <-botDone }()
	scope := attached.Scope
	request := permitRequest{Type: "permit_request", SessionID: id, Epoch: scope.Epoch, Generation: scope.Generation}
	for {
		request.Nonce = nonce()
		require.NoError(t, websocket.JSON.Send(registry, request))
		var reply struct {
			permitRequest
			Remaining int64 `json:"remaining_ms"`
		}
		require.NoError(t, websocket.JSON.Receive(registry, &reply))
		require.Equal(t, request.Nonce, reply.Nonce)
		if reply.Remaining > 0 {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	revoked := make(chan bool, 1)
	go func() {
		confirmed, e := b.Revoke(ctx, scope, scope.Epoch, scope.Generation+1)
		revoked <- confirmed && e == nil
	}()
	var transition revocation
	require.NoError(t, websocket.JSON.Receive(registry, &transition))
	transition.Type = "revoked"
	require.NoError(t, websocket.JSON.Send(registry, transition))
	require.True(t, <-revoked)
	request.Nonce = nonce()
	require.NoError(t, websocket.JSON.Send(registry, request))
	var reply struct {
		Remaining int64 `json:"remaining_ms"`
	}
	require.NoError(t, websocket.JSON.Receive(registry, &reply))
	require.Zero(t, reply.Remaining)
	_, err = b.Revoke(ctx, scope, scope.Epoch, scope.Generation+2)
	require.ErrorIs(t, err, errProtocol, "retry cannot change transition arguments")
	until, err := b.worker.StopBridgePermits(scope)
	require.NoError(t, err)
	_, err = b.worker.ExportSession(id)
	require.ErrorIs(t, err, mediaworker.ErrExportNotFlushed, "a stopped generation cannot move without its revocation boundary")
	// Blackhole registry replies without closing either socket. Revoke retries
	// must use the same transition and cannot extend the grant-stop deadline.
	confirmed, err := b.Revoke(ctx, scope, scope.Epoch, scope.Generation+1)
	require.NoError(t, err)
	require.False(t, confirmed)
	require.False(t, time.Now().Before(until), "unconfirmed revocation must wait the entire permit cap")
	require.WithinDuration(t, until, time.Now(), time.Second)
	require.Equal(t, 1, b.worker.SessionCount())
	require.NoError(t, registry.SetReadDeadline(time.Now().Add(time.Second)))
	request.Nonce = nonce()
	require.NoError(t, websocket.JSON.Send(registry, request))
	retries := 0
	for {
		var message struct {
			revocation
			Remaining int64 `json:"remaining_ms"`
		}
		require.NoError(t, websocket.JSON.Receive(registry, &message))
		if message.Type == "permit" {
			require.Zero(t, message.Remaining)
			break
		}
		expected := transition
		expected.Type = "revoke"
		require.Equal(t, expected, message.revocation)
		retries++
	}
	require.Greater(t, retries, 1, "blackholed acknowledgements must cause bounded exact retries")
	t.Logf("permit_cap=%s fallback_lateness=%s call_retained=true", mediaworker.BridgePermitCap, time.Since(until))
}

func TestBridgeRevokeUnavailableNeverAuthorizesWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _, id := authorityCall(t, ctx)
	epoch, err := b.worker.BridgeOutputEpoch(id)
	require.NoError(t, err)
	scope := mediaworker.BridgeOutputScope{SessionID: id, BotInstance: "bot", Epoch: epoch, Generation: 1, LineageID: "lineage"}
	require.NoError(t, b.worker.BindBridgeOutput(scope))
	stopped, cancelStop := context.WithCancel(ctx)
	cancelStop()
	confirmed, err := b.Revoke(stopped, scope, epoch, 2)
	require.ErrorIs(t, err, ErrBotUnavailable)
	require.False(t, confirmed)
	require.Zero(t, b.worker.BridgePermitRemaining(scope))
	require.Equal(t, 1, b.worker.SessionCount())
}
