package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

type dcUpstream struct {
	url      string
	frames   chan []byte
	sessions chan string
	sends    chan []byte
	close    chan struct{}
	once     sync.Once
}

func newDCUpstream(t *testing.T) *dcUpstream {
	t.Helper()
	u := &dcUpstream{frames: make(chan []byte, 4096), sessions: make(chan string, 128), sends: make(chan []byte, 4096), close: make(chan struct{})}
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		require.NotEmpty(t, ws.Request().URL.Query().Get("session"))
		require.NotEmpty(t, ws.Request().URL.Query().Get("worker"))
		u.sessions <- ws.Request().URL.Query().Get("session")
		defer ws.Close()
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case frame := <-u.sends:
					if websocket.Message.Send(ws, frame) != nil {
						return
					}
				case <-done:
					return
				case <-u.close:
					_ = ws.Close()
					return
				}
			}
		}()
		for {
			var frame []byte
			if websocket.Message.Receive(ws, &frame) != nil {
				return
			}
			u.frames <- frame
			if len(frame) > 0 && frame[0] == 0 {
				if websocket.Message.Send(ws, frame) != nil {
					return
				}
			}
		}
	}))
	u.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() { u.once.Do(func() { close(u.close) }); server.Close() })
	return u
}
func dcFrame(op byte, label string, payload []byte) []byte {
	return append(append([]byte{op, byte(len(label))}, []byte(label)...), payload...)
}

type dcSystem struct {
	plane    *controlplane.Plane
	workers  []*mediaworker.Worker
	signal   string
	client   *httptest.Server
	upstream *dcUpstream
	relay    *relay.Relay
	store    sessionstore.Store
}

func newDCSystem(t *testing.T, enabled bool, maxPending int) *dcSystem {
	t.Helper()
	u := newDCUpstream(t)
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store, Buffer: &relay.BufferConfig{}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p := controlplane.New(r, store)
	sys := &dcSystem{plane: p, upstream: u, relay: r, store: store}
	for i := 0; i < 2; i++ {
		cfg := mediaworker.Config{Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}}
		if enabled {
			cfg.DataChannel = &mediaworker.DataChannelConfig{Upstream: u.url, WorkerName: fmt.Sprint(i), MaxPending: maxPending}
		}
		w, err := mediaworker.New(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		r.AddWorker(w.LocalAddr())
		server := httptest.NewServer(w.PrivateHandler())
		t.Cleanup(server.Close)
		require.NoError(t, p.Register(fmt.Sprint(i), w.LocalAddr(), &controlplane.RemoteWorker{URL: server.URL}))
		// The remote worker boundary is real HTTP; embedded heartbeat only keeps
		// this short fixture healthy. The process test below owns real heartbeats.
		w.StartHeartbeats(p)
		sys.workers = append(sys.workers, w)
	}
	srv, client := frontend(t, p.Handler())
	sys.signal = srv.URL
	sys.client = srv
	// frontend uses the actual relais-signal handler including bearer auth.
	_ = client
	return sys
}

type dcCaller struct {
	pc       *webrtc.PeerConnection
	channels [2]*webrtc.DataChannel
	received [2]chan uint32
	closed   [2]chan struct{}
	id       string
	audio    atomic.Uint64
	cancel   context.CancelFunc
}

func dialDC(t *testing.T, sys *dcSystem, audio bool) *dcCaller {
	return dialDCOnWorker(t, sys, audio, "0", 1024)
}
func dialDCOnWorker(t *testing.T, sys *dcSystem, audio bool, worker string, peerLimit int) *dcCaller {
	t.Helper()
	c := &dcCaller{}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	c.pc = pc
	t.Cleanup(func() {
		if c.cancel != nil {
			c.cancel()
		}
		_ = pc.Close()
	})
	for i, label := range []string{"control", "state"} {
		opts := &webrtc.DataChannelInit{}
		if i == 1 {
			ordered := false
			retries := uint16(0)
			opts.Ordered = &ordered
			opts.MaxRetransmits = &retries
		}
		dc, err := pc.CreateDataChannel(label, opts)
		require.NoError(t, err)
		c.channels[i] = dc
		c.received[i] = make(chan uint32, 4096)
		c.closed[i] = make(chan struct{})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if len(msg.Data) >= 4 {
				c.received[i] <- binary.BigEndian.Uint32(msg.Data)
			}
		})
		dc.OnClose(func() { close(c.closed[i]) })
	}
	if audio {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "dc-test")
		require.NoError(t, err)
		_, err = pc.AddTrack(track)
		require.NoError(t, err)
		pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			for {
				_, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				c.audio.Add(1)
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_ = track.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
				}
			}
		}()
	}
	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	gather := webrtc.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(offer))
	<-gather
	// Force placement to worker zero using the allowed public signal parameter.
	resp, answer := request(t, sys.client.Client(), "POST", sys.signal+"/calls?worker="+worker, pc.LocalDescription().SDP, testToken)
	require.Equal(t, 201, resp.StatusCode, answer)
	c.id = strings.TrimPrefix(resp.Header.Get("Location"), "/calls/")
	require.Contains(t, answer, "a=sctp-port:5000")
	require.Contains(t, answer, "a=max-message-size:1024")
	require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: strings.ReplaceAll(answer, "a=max-message-size:1024", fmt.Sprintf("a=max-message-size:%d", peerLimit))}))
	require.Eventually(t, func() bool {
		return c.channels[0].ReadyState() == webrtc.DataChannelStateOpen && c.channels[1].ReadyState() == webrtc.DataChannelStateOpen
	}, 5*time.Second, 10*time.Millisecond)
	return c
}
func (c *dcCaller) roundtrip(t *testing.T, n int) {
	t.Helper()
	for j := 0; j < n; j++ {
		for i, dc := range c.channels {
			data := make([]byte, 4)
			binary.BigEndian.PutUint32(data, uint32(j))
			require.NoError(t, dc.Send(data))
			select {
			case got := <-c.received[i]:
				require.Equal(t, uint32(j), got)
			case <-time.After(2 * time.Second):
				t.Fatalf("channel %d message %d missing", i, j)
			}
		}
	}
}
func TestDataChannelSignalRelayRoundTrip(t *testing.T) {
	for _, audio := range []bool{false, true} {
		t.Run(fmt.Sprintf("audio=%t", audio), func(t *testing.T) {
			sys := newDCSystem(t, true, 0)
			c := dialDC(t, sys, audio)
			n := 100
			if !testing.Short() {
				n = 1000
			}
			c.roundtrip(t, n)
			if audio {
				require.Eventually(t, func() bool { return c.audio.Load() > 10 }, 2*time.Second, 10*time.Millisecond)
			}
			// Source preflight refuses before export, transfer, or relay hold.
			_, err := sys.plane.Move(context.Background(), c.id, "1")
			require.ErrorIs(t, err, mediaworker.ErrDataSessionNotMovable)
			c.roundtrip(t, 3)
			lease, err := sys.store.Get(context.Background(), c.id)
			require.NoError(t, err)
			require.Equal(t, sys.workers[0].LocalAddr(), lease.Worker)
			raw, err := sys.store.GetState(context.Background(), c.id)
			require.NoError(t, err)
			data, err := mediaworker.SnapshotIsDataSession(raw)
			require.NoError(t, err)
			require.True(t, data)
			_, err = sys.workers[1].ResumeSession(raw, mediaworker.ResumeOptions{})
			require.ErrorIs(t, err, mediaworker.ErrDataSessionReconnectRequired)
			_, err = sys.workers[0].ExportSession(c.id)
			require.ErrorIs(t, err, mediaworker.ErrDataSessionNotMovable)
			// Drain must terminate rather than export this session.
			results, err := sys.plane.Drain(context.Background(), "0")
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Equal(t, "ended", results[0].Kind)
			require.Eventually(t, func() bool {
				for {
					select {
					case f := <-sys.upstream.frames:
						if len(f) > 2 && f[0] == 3 {
							var end struct{ Reason string }
							require.NoError(t, json.Unmarshal(f[2:], &end))
							return end.Reason == "reconnect_required"
						}
					default:
						return false
					}
				}
			}, 2*time.Second, 10*time.Millisecond)
			require.Zero(t, sys.workers[0].SessionCount())
		})
	}
}
func TestDataChannelUpstreamClosePreservesAudio(t *testing.T) {
	sys := newDCSystem(t, true, 0)
	c := dialDC(t, sys, true)
	c.roundtrip(t, 2)
	require.Eventually(t, func() bool { return c.audio.Load() > 5 }, time.Second, 10*time.Millisecond)
	sys.upstream.once.Do(func() { close(sys.upstream.close) })
	for i := range c.channels {
		select {
		case <-c.closed[i]:
		case <-time.After(3 * time.Second):
			t.Fatal("channel did not close")
		}
	}
	before := c.audio.Load()
	require.Eventually(t, func() bool { return c.audio.Load() > before+5 }, time.Second, 10*time.Millisecond)
	require.Equal(t, 1, sys.workers[0].SessionCount())
}
func TestDataChannelOversizeAndPendingBoundsThroughRelay(t *testing.T) {
	for _, audio := range []bool{false, true} {
		for _, label := range []string{"control", "state"} {
			for _, oversize := range []bool{true, false} {
				t.Run(fmt.Sprintf("audio=%t/%s/oversize=%t", audio, label, oversize), func(t *testing.T) {
					sys := newDCSystem(t, true, 1024)
					c := dialDC(t, sys, audio)
					c.roundtrip(t, 1)
					size := 1024
					if oversize {
						size++
					}
					count := 100
					if oversize {
						count = 1
					}
					for i := range count {
						payload := make([]byte, size)
						binary.BigEndian.PutUint32(payload, uint32(i))
						sys.upstream.sends <- dcFrame(0, label, payload)
					}
					require.Eventually(t, func() bool {
						s := sys.workers[0].DataChannelStats()
						if oversize {
							return s.Oversized > 0
						}
						if label == "control" {
							return s.PendingOverflow > 0
						}
						return s.DroppedOldest > 0
					}, 3*time.Second, 10*time.Millisecond)
					if label == "control" {
						select {
						case <-c.closed[0]:
						case <-time.After(3 * time.Second):
							t.Fatal("reliable channel must close")
						}
						expected := "datachannel_pending_overflow"
						if oversize {
							expected = "datachannel_message_too_large"
						}
						require.Eventually(t, func() bool {
							for {
								select {
								case f := <-sys.upstream.frames:
									if len(f) > 2 && f[0] == 2 && strings.Contains(string(f), expected) {
										return true
									}
								default:
									return false
								}
							}
						}, time.Second, 10*time.Millisecond)
					} else {
						require.Equal(t, webrtc.DataChannelStateOpen, c.channels[1].ReadyState())
					}
					if label == "state" && !oversize {
						// The newest message must survive overflow. A drop-newest
						// implementation is red even if its drop counter is green.
						require.Eventually(t, func() bool {
							for {
								select {
								case seq := <-c.received[1]:
									if seq == uint32(count-1) {
										return true
									}
								default:
									return false
								}
							}
						}, 2*time.Second, 10*time.Millisecond)
					}
					if audio {
						before := c.audio.Load()
						require.Eventually(t, func() bool { return c.audio.Load() > before+3 }, time.Second, 10*time.Millisecond)
					}
				})
			}
		}
	}
}

func TestDataChannelClientOversizeTypedClose(t *testing.T) {
	sys := newDCSystem(t, true, 0)
	c := dialDCOnWorker(t, sys, false, "0", 65536)
	require.NoError(t, c.channels[0].Send(make([]byte, 1025)))
	select {
	case <-c.closed[0]:
	case <-time.After(3 * time.Second):
		t.Fatal("oversized client message did not close reliable channel")
	}
	require.EqualValues(t, 1, sys.workers[0].DataChannelStats().Oversized)
	require.Equal(t, webrtc.DataChannelStateOpen, c.channels[1].ReadyState())
}
