package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "test-secret-token-with-at-least-32-characters"

func frontend(t *testing.T, upstream http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	return frontendWithClock(t, upstream, time.Now)
}

func frontendWithClock(t *testing.T, upstream http.Handler, now func() time.Time) (*httptest.Server, *http.Client) {
	t.Helper()
	private := httptest.NewServer(upstream)
	t.Cleanup(private.Close)
	target, err := url.Parse(private.URL)
	require.NoError(t, err)
	transport := upstreamTransport()
	t.Cleanup(transport.CloseIdleConnections)
	cert, _, _, err := certificate(config{AdvertiseIPs: "127.0.0.1"})
	require.NoError(t, err)
	handler, err := newHandlerWithClock(target, testToken, transport, now)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config = newServer(srv.Config.Handler, cert)
	srv.TLS = srv.Config.TLSConfig
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	clientTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	t.Cleanup(clientTransport.CloseIdleConnections)
	client := &http.Client{Transport: clientTransport, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return srv, client
}

func request(t *testing.T, client *http.Client, method, endpoint, body, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := client.Do(req)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(data)
}

func TestCallThroughFrontendToRealControlPlane(t *testing.T) {
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	r.AddWorker(worker.LocalAddr())
	plane := controlplane.New(r, store)
	require.NoError(t, plane.Register("0", worker.LocalAddr(), worker))
	srv, client := frontend(t, plane.Handler())

	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pc.Close()) })
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "test")
	require.NoError(t, err)
	_, err = pc.AddTrack(track)
	require.NoError(t, err)
	echoed := make(chan *rtp.Packet, 1)
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		packet, _, err := remote.ReadRTP()
		if err == nil {
			select {
			case echoed <- packet:
			default:
			}
		}
	})
	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(offer))
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("ICE gathering timeout")
	}
	resp, answer := request(t, client, http.MethodPost, srv.URL+"/calls", pc.LocalDescription().SDP, testToken)
	require.Equal(t, http.StatusCreated, resp.StatusCode, answer)
	location := resp.Header.Get("Location")
	require.True(t, callPath(location))
	require.NoError(t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}))
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("WebRTC connection timeout")
	}
	for seq := uint16(1); seq <= 20; seq++ {
		require.NoError(t, track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}}))
		select {
		case packet := <-echoed:
			require.Equal(t, []byte{0xf8, 0xff, 0xfe}, packet.Payload)
			goto received
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("no echoed media through the relay")
received:
	resp, body := request(t, client, http.MethodGet, srv.URL+"/status", "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var status controlplane.Status // Existing callers can still decode the view.
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Len(t, status.Calls, 1)
	require.Equal(t, strings.TrimPrefix(location, "/calls/"), status.Calls[0].ID)
	require.Equal(t, "0", status.Calls[0].Owner)
	require.NotContains(t, body, worker.LocalAddr().String())
	require.NotContains(t, body, "owner_address")
	require.NotContains(t, body, `"address"`)
	resp, body = request(t, client, http.MethodDelete, srv.URL+location, "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = request(t, client, http.MethodGet, srv.URL+"/status", "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Empty(t, status.Calls)
}

func TestAuthenticationAndRefusedRoutes(t *testing.T) {
	var forwarded atomic.Int32
	srv, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(http.StatusOK) }))
	for _, path := range []string{"/calls", "/calls/id", "/status", "/clock", "/metrics", "/workers/a/drain"} {
		for _, token := range []string{"", "wrong", testToken + "x", testToken[:len(testToken)-1], strings.Repeat("x", len(testToken))} {
			resp, _ := request(t, client, http.MethodGet, srv.URL+path, "", token)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/calls/id/move"}, {"POST", "/workers/a/drain"}, {"POST", "/demo/kill"}, {"POST", "/demo/register"}, {"POST", "/workers"}, {"GET", "/metrics"}, {"GET", "/"},
		{"GET", "/calls"}, {"POST", "/calls/id"}, {"GET", "/calls/id"}, {"POST", "/status"}, {"DELETE", "/clock"}, {"HEAD", "/clock"}, {"OPTIONS", "/calls"},
		{"DELETE", "/calls/../status"}, {"DELETE", "/calls/%2e%2e%2fstatus"}, {"DELETE", "/calls/%69d"}, {"POST", "/calls/"}, {"GET", "//status"}, {"GET", "/status/"},
	} {
		resp, _ := request(t, client, tc.method, srv.URL+tc.path, "", testToken)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode, tc)
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/clock", nil)
	require.NoError(t, err)
	req.Header.Add("Authorization", "Bearer "+testToken)
	req.Header.Add("Authorization", "Bearer "+testToken)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Zero(t, forwarded.Load())
}

func TestSanitizedStatusAndClock(t *testing.T) {
	now := time.Now().UTC()
	secret := "10.42.0.9:9300"
	in := controlplane.Status{
		Workers:   []controlplane.WorkerStatus{{Name: secret, Address: netip.MustParseAddrPort(secret), Dead: true}},
		Calls:     []controlplane.CallStatus{{ID: "publiccall", Owner: secret, Address: netip.MustParseAddrPort(secret), Epoch: 4, MoveCount: 2, TakeoverCount: 1}},
		LostCount: 1,
		Takeovers: []controlplane.MoveResult{{ID: "publiccall", Kind: "takeover", From: secret, To: "1", Start: now, End: now.Add(time.Second), Lost: true, LossCause: "checkpoint_unsafe", Error: "dial tcp " + secret + ": failed", Result: mediaworker.HandoverResult{SessionID: secret, MoveDiagnosticID: secret, Duration: time.Second, RelayReplayDrops: 3}}},
	}
	srv, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, in) }))
	resp, body := request(t, client, http.MethodGet, srv.URL+"/status", "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, body, "10.42.0.9")
	require.NotContains(t, body, "address")
	require.NotContains(t, body, "dial tcp")
	var out controlplane.Status
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, "publiccall", out.Calls[0].ID)
	require.Equal(t, out.Calls[0].Owner, out.Workers[0].Name)
	require.Equal(t, out.Calls[0].Owner, out.Takeovers[0].From)
	require.True(t, out.Takeovers[0].Lost)
	require.Equal(t, "checkpoint_unsafe", out.Takeovers[0].LossCause)
	require.Equal(t, "operation failed", out.Takeovers[0].Error)
	require.Equal(t, time.Second, out.Takeovers[0].Result.Duration)
	require.Equal(t, 3, out.Takeovers[0].Result.RelayReplayDrops)
	require.Equal(t, uint64(1), out.LostCount)
	before := time.Now()
	resp, body = request(t, client, http.MethodGet, srv.URL+"/clock", "", testToken)
	after := time.Now()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var sample clockSample
	require.NoError(t, json.Unmarshal([]byte(body), &sample))
	require.False(t, sample.ReceivedAt.Before(before))
	require.False(t, sample.SentAt.Before(sample.ReceivedAt))
	require.False(t, sample.SentAt.After(after))
}

func TestRequestLimitsAndProxyBoundary(t *testing.T) {
	var forwarded atomic.Int32
	srv, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("X-Forwarded-For"))
		assert.Empty(t, r.Header.Get("X-Forwarded-Host"))
		assert.Empty(t, r.Header.Get("X-Forwarded-Proto"))
		assert.Empty(t, r.Trailer)
		assert.Empty(t, r.Header.Get("Cookie"))
		assert.Equal(t, "/calls", r.URL.Path)
		w.Header().Set("Location", "http://10.42.0.9:9200/calls/id")
		w.Header().Set("X-Private", "10.42.0.9")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, "dial tcp 10.42.0.9:9200: failure")
	}))
	for _, chunked := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/calls", strings.NewReader(strings.Repeat("x", maxRequestBytes+1)))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+testToken)
		if chunked {
			req.ContentLength = -1
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	}
	require.Zero(t, forwarded.Load(), "oversized offers must never reach control")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/calls", strings.NewReader("offer"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Cookie", "secret=1")
	req.Header.Set("X-Forwarded-For", "10.42.0.9")
	req.Header.Set("X-Forwarded-Host", "private")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Private-Trailer": []string{"secret"}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Location"))
	require.Empty(t, resp.Header.Get("X-Private"))
	require.NotContains(t, string(body), "10.42.0.9")
	require.Contains(t, string(body), "uncertain")
}

func TestUpstreamFailuresAreSanitized(t *testing.T) {
	for _, name := range []string{"redirect", "bad-status", "large-status", "large-answer", "private-location", "null-status", "empty-status", "private-call-id"} {
		t.Run(name, func(t *testing.T) {
			srv, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch name {
				case "redirect":
					w.Header().Set("Location", "http://10.42.0.9/private")
					w.WriteHeader(http.StatusFound)
				case "null-status":
					_, _ = io.WriteString(w, "null")
				case "empty-status":
					_, _ = io.WriteString(w, "{}")
				case "private-call-id":
					_, _ = io.WriteString(w, `{ "workers": [], "calls": [{"id": "10.42.0.9:9300"}], "takeovers": [] }`)
				case "bad-status":
					_, _ = io.WriteString(w, "10.42.0.9 is not JSON")
				case "large-status":
					_, _ = io.WriteString(w, strings.Repeat("x", maxStatusBytes+1))
				case "large-answer":
					w.Header().Set("Location", "/calls/id")
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, strings.Repeat("x", maxRequestBytes+1))
				case "private-location":
					w.Header().Set("Location", "http://10.42.0.9/calls/id")
					w.WriteHeader(http.StatusCreated)
				}
			}))
			method, path := http.MethodGet, "/status"
			if name == "large-answer" || name == "private-location" {
				method, path = http.MethodPost, "/calls"
			}
			resp, body := request(t, client, method, srv.URL+path, "", testToken)
			require.Equal(t, http.StatusBadGateway, resp.StatusCode)
			require.NotContains(t, body, "10.42.0.9")
			require.Empty(t, resp.Header.Get("Location"))
		})
	}
}

func TestUpstreamCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	srv, client := frontend(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/status", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream not started")
	}
	cancel()
	require.Error(t, <-done)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not cancel")
	}
}

func TestUpstreamTimeout(t *testing.T) {
	canceled := make(chan struct{})
	srv, client := frontend(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	client.Timeout = requestTimeout + 5*time.Second
	start := time.Now()
	resp, body := request(t, client, http.MethodGet, srv.URL+"/status", "", testToken)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Equal(t, "status unavailable\n", body)
	require.Less(t, time.Since(start), requestTimeout+3*time.Second)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out request was not canceled at control")
	}
}

func TestQueryBoundary(t *testing.T) {
	var forwarded atomic.Int32
	srv, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		if r.Method == http.MethodPost {
			assert.Equal(t, "worker=worker_1", r.URL.RawQuery)
			w.Header().Set("Location", "/calls/id")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "answer")
		} else {
			assert.Empty(t, r.URL.RawQuery)
			assert.False(t, r.URL.ForceQuery)
			w.WriteHeader(http.StatusOK)
		}
	}))
	for _, tc := range []struct{ method, path string }{
		{"POST", "/calls?debug=1"}, {"POST", "/calls?worker=0&debug=1"},
		{"POST", "/calls?worker=0&worker=1"}, {"POST", "/calls?worker="},
		{"POST", "/calls?worker=10.42.0.9%3A9300"}, {"POST", "/calls?worker=../status"},
		{"POST", "/calls?worker=" + strings.Repeat("a", 257)},
		{"POST", "/calls?worker=%zz"}, {"POST", "/calls?worker=0;debug=1"},
		{"DELETE", "/calls/id?worker=0"}, {"DELETE", "/calls/id?debug=1"},
		{"GET", "/status?debug=1"}, {"GET", "/clock?worker=0"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			resp, _ := request(t, client, tc.method, srv.URL+tc.path, "offer", testToken)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	require.Zero(t, forwarded.Load(), "invalid queries must never reach control")
	resp, _ := request(t, client, http.MethodPost, srv.URL+"/calls?%77orker=worker%5F1", "offer", testToken)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp, _ = request(t, client, http.MethodDelete, srv.URL+"/calls/id?", "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(2), forwarded.Load())
}

func TestGeneralOPTIONSRequiresAuthentication(t *testing.T) {
	srv, client := frontend(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { t.Error("OPTIONS reached upstream") }))
	endpoint, err := url.Parse(srv.URL)
	require.NoError(t, err)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	for _, token := range []string{"", testToken} {
		conn, err := tls.Dial("tcp", endpoint.Host, transport.TLSClientConfig)
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
		header := ""
		if token != "" {
			header = "Authorization: Bearer " + token + "\r\n"
		}
		_, err = fmt.Fprintf(conn, "OPTIONS * HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n", endpoint.Host, header)
		require.NoError(t, err)
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodOptions})
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, conn.Close())
		expected := http.StatusUnauthorized
		if token != "" {
			expected = http.StatusNotFound
		}
		require.Equal(t, expected, resp.StatusCode)
	}
}

func TestWorkerAliasesAreScopedToFrontendRun(t *testing.T) {
	status := controlplane.Status{
		Workers:   []controlplane.WorkerStatus{{Name: "10.42.0.9:9300"}, {Name: "0"}},
		Calls:     []controlplane.CallStatus{{ID: "publiccall", Owner: "10.42.0.9:9300"}},
		Takeovers: []controlplane.MoveResult{},
	}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status) })
	first, firstClient := frontend(t, upstream)
	second, secondClient := frontend(t, upstream)
	read := func(srv *httptest.Server, client *http.Client) controlplane.Status {
		resp, body := request(t, client, http.MethodGet, srv.URL+"/status", "", testToken)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out controlplane.Status
		require.NoError(t, json.Unmarshal([]byte(body), &out))
		require.Equal(t, out.Workers[0].Name, out.Calls[0].Owner)
		require.Equal(t, "0", out.Workers[1].Name)
		return out
	}
	a := read(first, firstClient)
	b := read(first, firstClient)
	c := read(second, secondClient)
	require.Equal(t, a.Workers[0].Name, b.Workers[0].Name)
	require.NotEqual(t, a.Workers[0].Name, c.Workers[0].Name)
}
