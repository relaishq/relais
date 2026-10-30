package agentbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// Default-run proof: real WebRTC/relay/leases, inbound runner sockets and
// token attachment. Each packet carries its bot sequence for wire uniqueness.
func TestBridgeRunnerMovesLiveCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	plane := controlplane.New(r, store)
	var workers []*mediaworker.Worker
	var mu sync.Mutex
	var issued uint64
	var lineage string
	var scopes []mediaworker.BridgeOutputScope
	pending := map[uint64][]byte{}
	errors := make(chan error, 16)
	bot := func(conn *websocket.Conn, m map[string]json.RawMessage) {
		mu.Lock()
		last := issued
		mu.Unlock()
		if e := attachFake(conn, m, last); e != nil {
			errors <- e
			return
		}
		var attached struct {
			Scope mediaworker.BridgeOutputScope `json:"scope"`
			Next  uint64                        `json:"next_output_seq"`
		}
		if e := websocket.JSON.Receive(conn, &attached); e != nil {
			errors <- e
			return
		}
		mu.Lock()
		scopes = append(scopes, attached.Scope)
		if lineage == "" {
			lineage = attached.Scope.LineageID
		}
		if lineage != attached.Scope.LineageID {
			mu.Unlock()
			errors <- fmt.Errorf("%w: bot lineage changed during attachment", errProtocol)
			return
		}
		for seq := range pending {
			if seq < attached.Next {
				delete(pending, seq)
			}
		}
		mu.Unlock()
		if e := websocket.JSON.Send(conn, map[string]any{"type": "output_begin", "request_id": "tone", "turn": 1}); e != nil {
			return
		}
		send := func(seq uint64, p []byte) error {
			raw := []byte{2}
			raw = binary.BigEndian.AppendUint64(raw, 1)
			raw = binary.BigEndian.AppendUint64(raw, seq)
			return websocket.Message.Send(conn, append(raw, p...))
		}
		begun := false
		for {
			var raw []byte
			if websocket.Message.Receive(conn, &raw) != nil {
				return
			}
			if len(raw) > 0 && raw[0] == 1 {
				if !begun {
					continue
				}
				mu.Lock()
				if issued >= 120 {
					mu.Unlock()
					continue
				}
				issued++
				seq := issued
				p := binary.BigEndian.AppendUint64(append([]byte{}, transportOpus...), seq)
				pending[seq] = p
				mu.Unlock()
				if send(seq, p) != nil {
					return
				}
			} else {
				var reply struct {
					Type    string `json:"type"`
					Seq     uint64 `json:"seq"`
					Outcome string `json:"outcome"`
					Reason  string `json:"reason"`
				}
				if json.Unmarshal(raw, &reply) != nil {
					return
				}
				if reply.Type == "output_begun" {
					begun = true
					mu.Lock()
					for seq := attached.Next; seq <= issued; seq++ {
						if p, ok := pending[seq]; ok {
							if send(seq, p) != nil {
								mu.Unlock()
								return
							}
						}
					}
					mu.Unlock()
				}
				if reply.Type == "output_receipt" {
					if reply.Outcome != "admitted" {
						errors <- fmt.Errorf("%w: output receipt seq=%d outcome=%q reason=%q", errProtocol, reply.Seq, reply.Outcome, reply.Reason)
						return
					}
					mu.Lock()
					delete(pending, reply.Seq)
					mu.Unlock()
				}
			}
		}
	}
	for _, name := range []string{"a", "b"} {
		b, e := New(Config{URL: "ws://127.0.0.1:9102/bridge", Token: "isolated-test-token"})
		require.NoError(t, e)
		f := agenthost.Factory(b.Registration())
		w, e := mediaworker.New(mediaworker.Config{SplitCheckpoints: true, Agent: mediaworker.AgentConfig{Factory: f, Registrations: map[string]legacy.Factory{"bridge": f}}, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, e)
		defer func() { require.NoError(t, w.Close()) }()
		workers = append(workers, w)
		b.SetWorker(w)
		r.AddWorker(w.LocalAddr())
		require.NoError(t, plane.Register(name, w.LocalAddr(), w))
		server := httptest.NewServer(b)
		defer server.Close()
		go func() { _ = fakeRunner(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge", bot) }()
	}
	pc, e := webrtc.NewAPI(webrtc.WithSettingEngine(func() webrtc.SettingEngine { var s webrtc.SettingEngine; s.SetIncludeLoopbackCandidate(true); return s }())).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, e)
	defer func() { require.NoError(t, pc.Close()) }()
	track, e := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "caller")
	require.NoError(t, e)
	_, e = pc.AddTrack(track)
	require.NoError(t, e)
	type arrival struct {
		packet *rtp.Packet
		at     time.Time
	}
	received := make(chan arrival, 256)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, e := track.ReadRTP()
			if e != nil {
				return
			}
			received <- arrival{p, time.Now()}
		}
	})
	gathered := webrtc.GatheringCompletePromise(pc)
	offer, e := pc.CreateOffer(nil)
	require.NoError(t, e)
	require.NoError(t, pc.SetLocalDescription(offer))
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	id, answer, e := plane.Create(ctx, pc.LocalDescription().SDP, "a")
	require.NoError(t, e)
	require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{Data: transportOpus, Duration: 20 * time.Millisecond})
			}
		}
	}()
	defer func() { cancel(); <-sent }()
	seen := map[uint64]bool{}
	target := "b"
	var prior time.Time
	var moveGaps []time.Duration
	for n := 1; n <= 120; n++ {
		select {
		case item := <-received:
			p := item.packet
			if n > 1 && (n-1)%30 == 0 {
				moveGaps = append(moveGaps, item.at.Sub(prior))
			}
			prior = item.at
			require.Len(t, p.Payload, len(transportOpus)+8)
			seq := binary.BigEndian.Uint64(p.Payload[len(transportOpus):])
			require.False(t, seen[seq], "duplicate bot packet on wire")
			seen[seq] = true
			require.EqualValues(t, n, seq)
			if n%30 == 0 && n < 120 {
				_, e = plane.Move(ctx, id, target)
				require.NoError(t, e)
				if target == "b" {
					target = "a"
				} else {
					target = "b"
				}
			}
		case e := <-errors:
			require.NoError(t, e)
		case <-ctx.Done():
			t.Fatalf("packets=%d: %v", len(seen), ctx.Err())
		}
	}
	for _, worker := range workers {
		metrics := httptest.NewRecorder()
		worker.PrivateHandler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Contains(t, metrics.Body.String(), "relais_worker_decryption_failures_total 0")
	}
	require.NoError(t, plane.End(ctx, id))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, scopes, 4)
	for i, s := range scopes {
		require.EqualValues(t, 1, s.Generation)
		if i > 0 {
			require.Greater(t, s.Epoch, scopes[i-1].Epoch)
		}
	}
	slices.Sort(moveGaps)
	t.Logf("local_move_gap_p95=%s samples=3 worker_decryption_failures=0", moveGaps[2])
	t.Logf("moves=3 packets=%d duplicate_packet_ids=0 scopes=%v", len(seen), scopes)
}
