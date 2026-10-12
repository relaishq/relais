package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/relayleg"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestTestRegistrationAndDemoAllowImmediatePlannedMove(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	relayServer := httptest.NewServer(r.PrivateHandler())
	t.Cleanup(relayServer.Close)
	remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
	plane := controlplane.New(remoteRelay, store)

	source, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	sourceAddress := source.LocalAddr().(*net.UDPAddr).AddrPort()
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method + " " + req.URL.Path {
		case "GET /status":
			privateapi.Write(w, mediaworker.WorkerStatus{Address: sourceAddress})
		case "POST /sessions":
			lease, claimErr := store.Claim(req.Context(), "existing-call", sourceAddress, time.Minute)
			require.NoError(t, claimErr)
			privateapi.Write(w, mediaworker.CreateReply{ID: lease.SessionID, Answer: "answer", Lease: lease})
		case "POST /sessions/existing-call/export":
			privateapi.Write(w, mediaworker.ExportReply{State: []byte("snapshot")})
		case "POST /resume": // Allows rollback on the unfixed registration path.
			privateapi.Write(w, mediaworker.ResumeReply{ID: "existing-call"})
		default:
			t.Errorf("unexpected source request: %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(sourceServer.Close)

	address := netip.MustParseAddrPort("10.0.0.10:41010")
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method + " " + req.URL.Path {
		case "GET /status":
			privateapi.Write(w, mediaworker.WorkerStatus{Address: address})
		case "POST /resume":
			var input mediaworker.ResumeRequest
			if privateapi.Read(w, req, &input) {
				require.Equal(t, []byte("snapshot"), input.State)
				require.Equal(t, address, input.Lease.Worker)
				privateapi.Write(w, mediaworker.ResumeReply{ID: input.Lease.SessionID})
			}
		default:
			t.Errorf("unexpected target request: %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(workerServer.Close)
	client := testWorkerClient()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, endpoint string) (net.Conn, error) {
		require.Equal(t, "10.0.0.10:9300", endpoint)
		return (&net.Dialer{}).DialContext(ctx, network, workerServer.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	var allowed testWorkerAllowlist
	require.NoError(t, allowed.Set("10.0.0.10=9300-9300,41010-41010"))
	server := httptest.NewServer(controlHandler(plane, remoteRelay, true, allowed, client))
	t.Cleanup(server.Close)
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/demo/register", map[string]string{
		"name": "source", "url": sourceServer.URL,
	}, nil, nil))
	id, _, err := plane.Create(ctx, "offer", "source")
	require.NoError(t, err)
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/private/test/register", testRegistrationRequest{
		Name: "fresh", URL: "http://10.0.0.10:9300", Address: address,
	}, nil, nil))

	// Use the real plane move, relay HTTP API and UDP drain barrier. Worker
	// snapshot export/resume is a fixture; no sync loop or sleep can mask readiness.
	require.NoError(t, source.SetReadDeadline(time.Now().Add(5*time.Second)))
	barrierDone := make(chan error, 1)
	go func() {
		packet := make([]byte, 64)
		n, sender, readErr := source.ReadFromUDPAddrPort(packet)
		if readErr != nil {
			barrierDone <- readErr
			return
		}
		barrierID, ack, ok := relayleg.ParseBarrier(packet[:n])
		if !ok || ack {
			barrierDone <- errors.New("expected relay drain barrier")
			return
		}
		_, writeErr := source.WriteToUDPAddrPort(relayleg.Barrier(barrierID, true), sender)
		barrierDone <- writeErr
	}()
	result, err := plane.Move(ctx, id, "fresh")
	require.NoError(t, err)
	require.Equal(t, "fresh", result.To)
	require.NoError(t, <-barrierDone)
	lease, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, address, lease.Worker)
}

func TestTestRegistrationRelayFailurePreventsSelection(t *testing.T) {
	remoteRelay := &controlplane.RemoteRelay{}
	plane := controlplane.New(remoteRelay, sessionstore.NewMemory())
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status, err := plane.Status(req.Context())
		require.NoError(t, err)
		require.Empty(t, status.Workers, "the plane must not select the worker before relay acceptance")
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(relayServer.Close)
	remoteRelay.URL = relayServer.URL
	address := netip.MustParseAddrPort("10.0.0.10:41010")
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, mediaworker.WorkerStatus{Address: address})
	}))
	t.Cleanup(workerServer.Close)
	client := testWorkerClient()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, workerServer.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	var allowed testWorkerAllowlist
	require.NoError(t, allowed.Set("10.0.0.10=9300-9300,41010-41010"))
	server := httptest.NewServer(controlHandler(plane, remoteRelay, false, allowed, client))
	t.Cleanup(server.Close)
	err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/private/test/register", testRegistrationRequest{
		Name: "fresh", URL: "http://10.0.0.10:9300", Address: address,
	}, nil, nil)
	var response *privateapi.HTTPError
	require.ErrorAs(t, err, &response)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.ErrorContains(t, err, "relay unavailable")
	status, err := plane.Status(context.Background())
	require.NoError(t, err)
	require.Empty(t, status.Workers)
}

func TestTestWorkerClientCountsHTTPBytes(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.TrafficCollectors()...)
	values := func() map[string]float64 {
		families, err := registry.Gather()
		require.NoError(t, err)
		result := make(map[string]float64)
		for _, family := range families {
			if family.GetName() != "relais_transport_bytes_total" {
				continue
			}
			for _, metric := range family.Metric {
				var transport, direction string
				for _, label := range metric.Label {
					switch label.GetName() {
					case "transport":
						transport = label.GetValue()
					case "direction":
						direction = label.GetValue()
					}
				}
				if transport == "http" {
					result[direction] += metric.Counter.GetValue()
				}
			}
		}
		return result
	}
	before := values()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "worker status")
	}))
	t.Cleanup(server.Close)
	client := testWorkerClient()
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get(server.URL + "/status")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	after := values()
	for _, direction := range []string{"in", "out"} {
		require.Greater(t, after[direction], before[direction], direction)
	}
}

func TestTestRegistrationNameBounds(t *testing.T) {
	var allowed testWorkerAllowlist
	require.NoError(t, allowed.Set("10.0.0.10=9300-9300,41010-41010"))
	request := testRegistrationRequest{URL: "http://10.0.0.10:9300", Address: netip.MustParseAddrPort("10.0.0.10:41010")}
	for _, name := range []string{"fresh", strings.Repeat("x", 64), "worker-é"} {
		request.Name = name
		require.NoError(t, allowed.validate(request))
	}
	for _, name := range []string{"", " ", strings.Repeat("x", 65), "line\nbreak", "tab\t", "null\x00", "escape\x1b", "delete\x7f", "hidden\u200b"} {
		request.Name = name
		require.Error(t, allowed.validate(request), "name: %q", name)
	}
}
