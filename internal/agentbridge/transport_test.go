package agentbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

var transportOpus = []byte{0xf8, 0xff, 0xfe}

type botResult struct {
	scope mediaworker.BridgeOutputScope
	err   error
}

// No Python/environment opt-in: make check runs the real run/write/emitUpdates
// path with Pion media, actual lease allocation and a fake WebSocket bot.
func TestBridgeTransportSequentialCallsUseLeaseEpoch(t *testing.T) {
	for _, preclaim := range []bool{false, true} {
		t.Run(fmt.Sprintf("preclaimed-%t", preclaim), func(t *testing.T) {
			store := sessionstore.NewMemory()
			if preclaim {
				_, err := store.Claim(context.Background(), "dummy", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
				require.NoError(t, err)
			}
			results := make(chan botResult, 2)
			server := httptest.NewServer(websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(conn *websocket.Conn) {
				defer func() { _ = conn.Close() }()
				scope, err := exerciseBot(conn)
				results <- botResult{scope, err}
			}})
			defer server.Close()
			b, err := New(Config{URL: "ws" + strings.TrimPrefix(server.URL, "http") + "/bridge", Token: "isolated-test-token"})
			require.NoError(t, err)
			r, err := relay.New(relay.Config{Owners: store})
			require.NoError(t, err)
			defer func() { require.NoError(t, r.Close()) }()
			factory := agenthost.Factory(b.Registration())
			w, err := mediaworker.New(mediaworker.Config{SplitCheckpoints: true, Agent: mediaworker.AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"bridge": factory}, CallbackTimeout: time.Second}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
			require.NoError(t, err)
			defer func() { require.NoError(t, w.Close()) }()
			b.SetWorker(w)
			r.AddWorker(w.LocalAddr())
			for call := range 2 {
				t.Run(fmt.Sprintf("call-%d", call+1), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					defer cancel()
					pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(func() webrtc.SettingEngine {
						var setting webrtc.SettingEngine
						setting.SetIncludeLoopbackCandidate(true)
						return setting
					}())).NewPeerConnection(webrtc.Configuration{})
					require.NoError(t, err)
					defer func() { require.NoError(t, pc.Close()) }()
					track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "caller")
					require.NoError(t, err)
					_, err = pc.AddTrack(track)
					require.NoError(t, err)
					received := make(chan *rtp.Packet, 1)
					pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
						p, _, err := track.ReadRTP()
						if err == nil {
							received <- p
						}
					})
					gathered := webrtc.GatheringCompletePromise(pc)
					offer, err := pc.CreateOffer(nil)
					require.NoError(t, err)
					require.NoError(t, pc.SetLocalDescription(offer))
					select {
					case <-gathered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					id, answer, err := w.CreateSession(ctx, pc.LocalDescription().SDP)
					require.NoError(t, err)
					defer func() { require.NoError(t, w.EndSession(id)) }()
					lease, err := store.Get(ctx, id)
					require.NoError(t, err)
					require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))
					sent := make(chan struct{})
					go func() {
						defer close(sent)
						ticker := time.NewTicker(20 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-ticker.C:
								_ = track.WriteSample(media.Sample{Data: transportOpus, Duration: 20 * time.Millisecond})
							}
						}
					}()
					defer func() { cancel(); <-sent }()
					select {
					case result := <-results:
						require.NoError(t, result.err)
						require.Equal(t, lease.Epoch, result.scope.Epoch)
						require.Equal(t, id, result.scope.SessionID)
						t.Logf("call=%d lease_epoch=%d protocol_error_closed=true", call+1, lease.Epoch)
					case <-ctx.Done():
						t.Fatal("bot transport did not finish")
					}
					select {
					case packet := <-received:
						require.Equal(t, transportOpus, packet.Payload)
					case <-ctx.Done():
						t.Fatal("bot audio did not reach Pion caller")
					}
				})
			}
		})
	}
}

func exerciseBot(conn *websocket.Conn) (mediaworker.BridgeOutputScope, error) {
	var scope mediaworker.BridgeOutputScope
	fail := func(err error) (mediaworker.BridgeOutputScope, error) { return scope, err }
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if conn.Request().Header.Get("Authorization") != "Bearer isolated-test-token" {
		return fail(fmt.Errorf("missing development authorization"))
	}
	send := func(v any) error { return websocket.JSON.Send(conn, v) }
	if err := send(map[string]any{"type": "hello", "protocol": 1, "bot_instance": "go-bot"}); err != nil {
		return fail(err)
	}
	var attached struct {
		Type  string                        `json:"type"`
		Scope mediaworker.BridgeOutputScope `json:"scope"`
	}
	if err := websocket.JSON.Receive(conn, &attached); err != nil {
		return fail(fmt.Errorf("attached: %w", err))
	}
	scope = attached.Scope
	if attached.Type != "attached" || scope.Epoch == 0 {
		return fail(fmt.Errorf("invalid attachment"))
	}
	receive := func() (map[string]json.RawMessage, error) {
		var raw []byte
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 && raw[0] == 1 {
			return map[string]json.RawMessage{"type": json.RawMessage(`"input"`)}, nil
		}
		var m map[string]json.RawMessage
		err := json.Unmarshal(raw, &m)
		return m, err
	}
	kind := func(m map[string]json.RawMessage) string { var k string; _ = json.Unmarshal(m["type"], &k); return k }
	for {
		m, err := receive()
		if err != nil {
			return fail(err)
		}
		if kind(m) == "input" {
			break
		}
	}
	if err := send(map[string]any{"type": "output_begin", "turn": 1, "request_id": "go-tone"}); err != nil {
		return fail(err)
	}
	for {
		m, err := receive()
		if err != nil {
			return fail(err)
		}
		if kind(m) == "output_begun" {
			break
		}
	}
	packet := []byte{2}
	packet = binary.BigEndian.AppendUint64(packet, 1)
	packet = binary.BigEndian.AppendUint64(packet, 1)
	packet = append(packet, transportOpus...)
	if err := websocket.Message.Send(conn, packet); err != nil {
		return fail(err)
	}
	if err := send(map[string]any{"type": "audio_end", "turn": 1, "last_seq": 1}); err != nil {
		return fail(err)
	}
	if err := send(checkpoint{Type: "checkpoint", Sequence: 1, Version: 1, Working: []byte("Go bot checkpoint")}); err != nil {
		return fail(err)
	}
	receipt, finished, checkpointed := false, false, false
	for !receipt || !finished || !checkpointed {
		m, err := receive()
		if err != nil {
			return fail(err)
		}
		switch kind(m) {
		case "output_receipt":
			var outcome string
			_ = json.Unmarshal(m["outcome"], &outcome)
			if outcome != "admitted" {
				return fail(fmt.Errorf("output outcome %s", outcome))
			}
			receipt = true
		case "playback_finished":
			var outcome string
			_ = json.Unmarshal(m["outcome"], &outcome)
			if outcome != "complete" {
				return fail(fmt.Errorf("playback outcome %s", outcome))
			}
			finished = true
		case "checkpointed":
			var seq uint64
			var working []byte
			if err := json.Unmarshal(m["seq"], &seq); err != nil {
				return fail(err)
			}
			if err := json.Unmarshal(m["working"], &working); err != nil {
				return fail(err)
			}
			if seq != 1 || !bytes.Equal(working, []byte("Go bot checkpoint")) {
				return fail(fmt.Errorf("wrong acknowledged pair"))
			}
			checkpointed = true
		}
	}
	if err := send(map[string]any{"type": "unsupported-protocol-message"}); err != nil {
		return fail(err)
	}
	for {
		if _, err := receive(); err != nil {
			if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
				return fail(fmt.Errorf("protocol error did not close socket"))
			}
			return scope, nil
		}
	}
}
