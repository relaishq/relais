package agentbridge

import (
	"context"
	"encoding/json"
	"net/http"
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

// These faults use real call ownership, host tasks and authenticated sockets.
// The fallback is explicit no-capacity/silence, not invented bot speech or Echo.
func TestBridgeAttachFaultHasBoundedCallFallback(t *testing.T) {
	for _, fault := range []string{"no-runner", "never-attach", "attach-crash"} {
		t.Run(fault, func(t *testing.T) {
			b, err := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			server := httptest.NewServer(b)
			defer server.Close()
			runnerDone := make(chan struct{})
			if fault != "no-runner" {
				go func() {
					defer close(runnerDone)
					_ = fakeRunner(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge", func(c *websocket.Conn, m map[string]json.RawMessage) {
						if fault == "attach-crash" {
							_ = attachFake(c, m, 0)
						}
					})
				}()
				defer func() { cancel(); <-runnerDone }()
			}
			store := sessionstore.NewMemory()
			rel, err := relay.New(relay.Config{Owners: store})
			require.NoError(t, err)
			defer func() { require.NoError(t, rel.Close()) }()
			factory := agenthost.Factory(b.Registration())
			w, err := mediaworker.New(mediaworker.Config{SplitCheckpoints: true, Agent: mediaworker.AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"bridge": factory}, CallbackTimeout: time.Second}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: rel.WorkerAddr(), PublicAddr: rel.PublicAddr()}})
			require.NoError(t, err)
			defer func() { require.NoError(t, w.Close()) }()
			b.SetWorker(w)
			rel.AddWorker(w.LocalAddr())
			var setting webrtc.SettingEngine
			setting.SetIncludeLoopbackCandidate(true)
			pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(setting)).NewPeerConnection(webrtc.Configuration{})
			require.NoError(t, err)
			defer func() { require.NoError(t, pc.Close()) }()
			_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
			require.NoError(t, err)
			gather := webrtc.GatheringCompletePromise(pc)
			offer, err := pc.CreateOffer(nil)
			require.NoError(t, err)
			require.NoError(t, pc.SetLocalDescription(offer))
			<-gather
			id, answer, err := w.CreateSession(ctx, pc.LocalDescription().SDP)
			require.NoError(t, err)
			defer func() { require.NoError(t, w.EndSession(id)) }()
			require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))
			started := time.Now()
			var outcome string
			require.Eventually(t, func() bool {
				response := httptest.NewRecorder()
				w.PrivateHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status?agent-session="+id, nil))
				var status mediaworker.WorkerStatus
				if json.Unmarshal(response.Body.Bytes(), &status) != nil || status.AgentSession == nil {
					return false
				}
				var state map[string]any
				_ = json.Unmarshal(status.AgentSession.App, &state)
				outcome, _ = state["bot_outcome"].(string)
				return outcome == "no_capacity_silence"
			}, 5*time.Second, 10*time.Millisecond)
			require.Less(t, time.Since(started), 4*time.Second, "2 s attach budget plus host settlement margin")
			require.Equal(t, 1, w.SessionCount(), "fallback must retain the caller")
			b.mu.Lock()
			require.Empty(t, b.pending)
			require.Empty(t, b.active)
			b.mu.Unlock()
			t.Logf("fault=%s outcome=%s elapsed=%s call_retained=true", fault, outcome, time.Since(started))
		})
	}
}
