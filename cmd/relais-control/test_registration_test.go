package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestTestWorkerAllowlistFlags(t *testing.T) {
	var allowed testWorkerAllowlist
	fs := flag.NewFlagSet("registration", flag.ContinueOnError)
	fs.Var(&allowed, "test-register-worker", "allowed host and ports")
	require.NoError(t, fs.Parse(nil))
	require.Empty(t, allowed)
	require.NoError(t, fs.Parse([]string{
		"-test-register-worker", "10.0.0.10=9300-9399,41010-41109",
		"-test-register-worker", "fd00::10=9400-9400,42000-42000",
	}))
	require.Len(t, allowed, 2)
	require.Equal(t, "10.0.0.10=9300-9399,41010-41109;fd00::10=9400-9400,42000-42000", allowed.String())
	require.NoError(t, allowed.validate(testRegistrationRequest{
		Name: "ipv6", URL: "http://[fd00::10]:9400", Address: netip.MustParseAddrPort("[fd00::10]:42000"),
	}))
	require.ErrorContains(t, allowed.Set("10.0.0.10=9500-9599,43000-43100"), "duplicate host")
	for _, input := range []string{
		"", "10.0.0.10", "localhost=9300-9399,41010-41109", "10.0.0.0/24=9300-9399,41010-41109",
		"127.0.0.1=9300-9399,41010-41109", "8.8.8.8=9300-9399,41010-41109",
		"0.0.0.0=9300-9399,41010-41109", "::1=9300-9399,41010-41109",
		"fd00::10%zone=9300-9399,41010-41109", "::ffff:10.0.0.10=9300-9399,41010-41109",
		"10.0.0.11=9300-9399", "10.0.0.11=0-9399,41010-41109", "10.0.0.11=9399-9300,41010-41109",
		"10.0.0.11=9300-65536,41010-41109", "10.0.0.11=9300,41010-41109", "10.0.0.11=9300-9399,0-41109",
		"10.0.0.11=9300-9399,41109-41010", "10.0.0.11=9300-9399,41010-65536",
		"10.0.0.11=9300-9399,41010-41109,42000-42001", "10.0.0.11=+9300-9399,41010-41109",
	} {
		t.Run(input, func(t *testing.T) {
			var invalid testWorkerAllowlist
			require.Error(t, invalid.Set(input))
			require.Empty(t, invalid)
		})
	}
}

func TestTestRegistrationDisabledByDefault(t *testing.T) {
	for _, demo := range []bool{false, true} {
		plane := controlplane.New(nil, sessionstore.NewMemory())
		handler := controlHandler(plane, nil, demo, nil, testWorkerClient())
		for _, path := range []string{"/private/test/register", "/demo/register"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			reply := httptest.NewRecorder()
			handler.ServeHTTP(reply, req)
			if demo && path == "/demo/register" {
				require.Equal(t, http.StatusBadRequest, reply.Code)
			} else {
				require.Equal(t, http.StatusNotFound, reply.Code)
			}
		}
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/status", nil))
		require.Equal(t, http.StatusOK, reply.Code)
	}
}

func TestTestRegistrationProductionFlag(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	for _, input := range [][]string{nil, {"-test-register-worker", "10.0.0.10=9300-9399,41010-41109", "-demo-register"}} {
		flag.CommandLine = flag.NewFlagSet("registration-test", flag.ContinueOnError)
		os.Args = append([]string{"registration-test", "-http", "0.0.0.0:0", "-relay", "http://127.0.0.1:1", "-worker", "0=http://127.0.0.1:2"}, input...)
		// Stop before opening a store or making any outbound connection.
		require.ErrorContains(t, run(), "private HTTP listener")
		option := flag.Lookup("test-register-worker")
		require.NotNil(t, option)
		require.Empty(t, option.DefValue)
		if len(input) == 0 {
			require.Empty(t, option.Value.String())
		} else {
			require.Equal(t, input[1], option.Value.String())
			require.Equal(t, "true", flag.Lookup("demo-register").Value.String())
		}
	}
}

func TestTestRegistrationBoundsAndUniqueIdentities(t *testing.T) {
	var allowed testWorkerAllowlist
	require.NoError(t, allowed.Set("10.0.0.10=9300-9302,41010-41012"))
	require.NoError(t, allowed.Set("10.0.0.11=9400-9400,42000-42000"))
	var workerCalls, relayCalls atomic.Int32
	var advertised atomic.Value
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerCalls.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/status", r.URL.Path)
		privateapi.Write(w, mediaworker.WorkerStatus{Address: advertised.Load().(netip.AddrPort)})
	}))
	t.Cleanup(workerServer.Close)
	relayAddresses := make(chan netip.AddrPort, 8)
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			privateapi.Write(w, relay.RelayStatus{Instance: "test"})
			return
		}
		relayCalls.Add(1)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/workers", r.URL.Path)
		var req relay.WorkerRequest
		if privateapi.Read(w, r, &req) {
			relayAddresses <- req.Address
			privateapi.Write(w, map[string]bool{"ok": true})
		}
	}))
	t.Cleanup(relayServer.Close)
	remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
	plane := controlplane.New(remoteRelay, sessionstore.NewMemory())
	client := testWorkerClient()
	transport := client.Transport.(*http.Transport)
	// Exercise the real HTTP client and route with literal private URLs. Only
	// the TCP dial maps those endpoints to an owned loopback test server.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		require.Contains(t, []string{"10.0.0.10:9300", "10.0.0.10:9302", "10.0.0.11:9400"}, address)
		return (&net.Dialer{}).DialContext(ctx, network, workerServer.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	server := httptest.NewServer(controlHandler(plane, remoteRelay, false, allowed, client))
	t.Cleanup(server.Close)
	request := func(name, endpoint, address string) testRegistrationRequest {
		return testRegistrationRequest{Name: name, URL: endpoint, Address: netip.MustParseAddrPort(address)}
	}
	good := request("fresh", "http://10.0.0.10:9300", "10.0.0.10:41010")
	type registrationCase struct {
		name       string
		req        testRegistrationRequest
		status     int
		probe      bool
		advertised string
		message    string
	}
	cases := []registrationCase{
		{name: "allowed lower bounds", req: good, status: http.StatusOK, probe: true},
		{name: "unlisted HTTP host", req: request("bad", "http://10.0.0.12:9300", "10.0.0.12:41010"), status: http.StatusBadRequest},
		{name: "public HTTP host", req: request("bad", "http://8.8.8.8:9300", "8.8.8.8:41010"), status: http.StatusBadRequest},
		{name: "HTTP below range", req: request("bad", "http://10.0.0.10:9299", "10.0.0.10:41010"), status: http.StatusBadRequest},
		{name: "HTTP above range", req: request("bad", "http://10.0.0.10:9303", "10.0.0.10:41010"), status: http.StatusBadRequest},
		{name: "HTTP port from other host", req: request("bad", "http://10.0.0.11:9300", "10.0.0.11:42000"), status: http.StatusBadRequest},
		{name: "UDP below range", req: request("bad", good.URL, "10.0.0.10:41009"), status: http.StatusBadRequest},
		{name: "UDP above range", req: request("bad", good.URL, "10.0.0.10:41013"), status: http.StatusBadRequest},
		{name: "different allowed media host", req: request("bad", good.URL, "10.0.0.11:42000"), status: http.StatusBadRequest},
		{name: "public media host", req: request("bad", good.URL, "8.8.8.8:41010"), status: http.StatusBadRequest},
		{name: "missing media address", req: testRegistrationRequest{Name: "bad", URL: good.URL}, status: http.StatusBadRequest},
		{name: "blank name", req: request(" ", good.URL, good.Address.String()), status: http.StatusBadRequest},
		{name: "status mismatch", req: request("bad", good.URL, "10.0.0.10:41011"), status: http.StatusConflict, probe: true, advertised: "10.0.0.10:41012", message: "does not match"},
		{name: "out of bounds status", req: request("bad", good.URL, "10.0.0.10:41011"), status: http.StatusConflict, probe: true, advertised: "8.8.8.8:41011", message: "does not match"},
		{name: "reused name", req: request("fresh", good.URL, "10.0.0.10:41011"), status: http.StatusConflict, probe: true, message: "duplicate worker name"},
		{name: "reused media address", req: request("other", good.URL, good.Address.String()), status: http.StatusConflict, probe: true, message: "duplicate worker address"},
		{name: "allowed upper bounds", req: request("upper", "http://10.0.0.10:9302", "10.0.0.10:41012"), status: http.StatusOK, probe: true},
		{name: "second allowed host", req: request("second", "http://10.0.0.11:9400", "10.0.0.11:42000"), status: http.StatusOK, probe: true},
	}
	for _, endpoint := range []string{
		"http://localhost:9300", "http://10.0.0.10", "https://10.0.0.10:9300", "http://user:pass@10.0.0.10:9300",
		"http://10.0.0.10:9300/private", "http://10.0.0.10:9300/", "http://10.0.0.10:9300?", "http://10.0.0.10:9300?q=1",
		"http://10.0.0.10:9300#fragment", "http://10.0.0.10:65536", "http://10.0.0.10:0",
	} {
		cases = append(cases, registrationCase{name: endpoint, req: request("bad", endpoint, good.Address.String()), status: http.StatusBadRequest})
	}
	for _, name := range []string{strings.Repeat("x", 65), "line\nbreak", "tab\t", "null\x00", "escape\x1b", "delete\x7f", "hidden\u200b"} {
		cases = append(cases, registrationCase{name: "invalid name " + name, req: request(name, good.URL, good.Address.String()), status: http.StatusBadRequest})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address := tc.req.Address
			if tc.advertised != "" {
				address = netip.MustParseAddrPort(tc.advertised)
			}
			advertised.Store(address)
			beforeWorkers, beforeRelay := workerCalls.Load(), relayCalls.Load()
			err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/private/test/register", tc.req, nil, nil)
			if tc.status == http.StatusOK {
				require.NoError(t, err)
			} else {
				var response *privateapi.HTTPError
				require.ErrorAs(t, err, &response)
				require.Equal(t, tc.status, response.StatusCode)
				if tc.message != "" {
					require.ErrorContains(t, err, tc.message)
				}
			}
			if tc.probe {
				require.Equal(t, beforeWorkers+1, workerCalls.Load())
			} else {
				require.Equal(t, beforeWorkers, workerCalls.Load())
			}
			if tc.probe && tc.advertised == "" {
				require.Equal(t, beforeRelay+1, relayCalls.Load(), "verified addresses reach the relay before identity checks")
			} else {
				require.Equal(t, beforeRelay, relayCalls.Load(), "invalid or mismatched addresses must not reach the relay")
			}
		})
	}
	status, err := plane.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Workers, 3)
	for _, worker := range status.Workers {
		require.Contains(t, []string{"fresh", "upper", "second"}, worker.Name)
		require.False(t, worker.Draining)
	}
	require.EqualValues(t, 5, relayCalls.Load())
	for range 5 {
		require.Contains(t, []netip.AddrPort{
			netip.MustParseAddrPort("10.0.0.10:41010"),
			netip.MustParseAddrPort("10.0.0.10:41011"), // Verified address from the rejected reused name.
			netip.MustParseAddrPort("10.0.0.10:41012"),
			netip.MustParseAddrPort("10.0.0.11:42000"),
		}, <-relayAddresses)
	}
	// Restart sync still discovers accepted registrations, including late workers.
	var registry controlplane.RelayWorkerRegistry
	require.NoError(t, registry.Sync(context.Background(), plane, remoteRelay))
	require.EqualValues(t, 8, relayCalls.Load())
	for range 3 {
		require.Contains(t, []netip.AddrPort{
			netip.MustParseAddrPort("10.0.0.10:41010"),
			netip.MustParseAddrPort("10.0.0.10:41012"),
			netip.MustParseAddrPort("10.0.0.11:42000"),
		}, <-relayAddresses)
	}
	require.NoError(t, registry.Sync(context.Background(), plane, remoteRelay))
	require.EqualValues(t, 8, relayCalls.Load())
}

func TestTestRegistrationClientRefusesRedirectsAndProxies(t *testing.T) {
	var escaped atomic.Int32
	escape := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		escaped.Add(1)
		privateapi.Write(w, mediaworker.WorkerStatus{Address: netip.MustParseAddrPort("10.0.0.10:41010")})
	}))
	t.Cleanup(escape.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, escape.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	t.Setenv("HTTP_PROXY", escape.URL)
	client := testWorkerClient()
	t.Cleanup(client.CloseIdleConnections)
	transport := client.Transport.(*http.Transport)
	require.Nil(t, transport.Proxy)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		require.Equal(t, "10.0.0.10:9300", address)
		return (&net.Dialer{}).DialContext(ctx, network, redirect.Listener.Addr().String())
	}
	var allowed testWorkerAllowlist
	require.NoError(t, allowed.Set("10.0.0.10=9300-9300,41010-41010"))
	var relayCalls atomic.Int32
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayCalls.Add(1)
		http.Error(w, "redirect must not register with the relay", http.StatusBadRequest)
	}))
	t.Cleanup(relayServer.Close)
	remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
	plane := controlplane.New(remoteRelay, sessionstore.NewMemory())
	server := httptest.NewServer(controlHandler(plane, remoteRelay, false, allowed, client))
	t.Cleanup(server.Close)
	err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/private/test/register", testRegistrationRequest{
		Name: "redirect", URL: "http://10.0.0.10:9300", Address: netip.MustParseAddrPort("10.0.0.10:41010"),
	}, nil, nil)
	require.ErrorContains(t, err, "302")
	require.Zero(t, escaped.Load())
	require.Zero(t, relayCalls.Load())
	status, err := plane.Status(context.Background())
	require.NoError(t, err)
	require.Empty(t, status.Workers)
	resp, err := server.Client().Get(server.URL + "/demo/register")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
