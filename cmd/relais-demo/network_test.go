package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/processrun"
	"github.com/stretchr/testify/require"
)

func TestLocalNetworkBindingRules(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "localhost", "example.com", "8.8.8.8", "::1", "fd00::5", "::ffff:127.0.0.1", "192.168.1.5:9000"} {
		require.Error(t, validateConfig(config{HTTP: "127.0.0.1:0", LocalNetwork: host}), host)
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "192.168.1.5"} {
		cfg := config{HTTP: "127.0.0.1:9201", LocalNetwork: host}
		require.NoError(t, validateConfig(cfg), host)
		require.Equal(t, net.JoinHostPort(host, "9201"), pageAddress(cfg))
		require.Equal(t, []string{"-media", net.JoinHostPort(host, "0"), "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0"}, relayArgs(cfg))
	}
	require.Equal(t, "127.0.0.1:9201", pageAddress(config{HTTP: "127.0.0.1:9201"}))
	require.Equal(t, "127.0.0.1:0", relayArgs(config{})[1])
	require.Error(t, validateConfig(config{HTTP: "0.0.0.0:9201", LocalNetwork: "192.168.1.5"}))
	require.Error(t, validateConfig(config{HTTP: "127.0.0.1:0", LocalNetwork: "127.0.0.1", Redis: "127.0.0.1:6379"}))
}

func TestLocalNetworkRejectsIPv6BeforeListening(t *testing.T) {
	for _, host := range []string{"::1", "fd00::5"} {
		listener, fingerprint, err := listenPage(config{HTTP: "127.0.0.1:0", LocalNetwork: host})
		require.ErrorContains(t, err, "IPv4")
		require.ErrorContains(t, err, "IPv6 media is not supported")
		require.Nil(t, listener)
		require.Empty(t, fingerprint)
		_, _, err = pageCertificate(host)
		require.Error(t, err)
	}
}

func TestPageCertificate(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "192.168.1.5"} {
		cert, fingerprint, err := pageCertificate(host)
		require.NoError(t, err)
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		require.NoError(t, err)
		require.IsType(t, &ecdsa.PrivateKey{}, cert.PrivateKey)
		require.Equal(t, x509.ECDSAWithSHA256, leaf.SignatureAlgorithm)
		require.NoError(t, leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature))
		require.NoError(t, leaf.VerifyHostname(host))
		require.Error(t, leaf.VerifyHostname("example.com"))
		require.Len(t, leaf.IPAddresses, 1)
		require.Empty(t, leaf.DNSNames)
		require.False(t, leaf.IsCA)
		require.WithinDuration(t, time.Now().Add(-time.Hour), leaf.NotBefore, 2*time.Second)
		require.WithinDuration(t, time.Now().Add(24*time.Hour), leaf.NotAfter, 2*time.Second)
		sum := sha256.Sum256(leaf.Raw)
		require.Equal(t, hex.EncodeToString(sum[:]), fingerprint)
		second, _, err := pageCertificate(host)
		require.NoError(t, err)
		require.NotEqual(t, cert.Certificate, second.Certificate)
	}
}

func TestLocalNetworkPageTLSAndOrigins(t *testing.T) {
	listener, fingerprint, err := listenPage(config{HTTP: "127.0.0.1:0", LocalNetwork: "127.0.0.1"})
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &demo{publicHost: "127.0.0.1", launchToken: "test-launch-token"}
	served := make(chan error, 1)
	go func() {
		served <- processrun.Serve(ctx, listener, d.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NotNil(t, r.TLS)
			w.WriteHeader(http.StatusNoContent)
		})))
	}()
	defer func() { cancel(); require.NoError(t, <-served) }()
	// This trust exception is limited to the owned, ephemeral demo listener.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	url := "https://" + listener.Addr().String()
	response, err := client.Get(url)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	sum := sha256.Sum256(response.TLS.PeerCertificates[0].Raw)
	require.Equal(t, hex.EncodeToString(sum[:]), fingerprint)
	_ = response.Body.Close()
	for _, tc := range []struct {
		host, origin string
		code         int
	}{
		{listener.Addr().String(), url, http.StatusNoContent},
		{listener.Addr().String(), "http://" + listener.Addr().String(), http.StatusForbidden},
		{listener.Addr().String(), "https://evil.example", http.StatusForbidden},
		{"evil.example", "https://evil.example", http.StatusForbidden},
		{"localhost", "https://localhost", http.StatusForbidden},
	} {
		req, err := http.NewRequest(http.MethodPost, url+"/unknown", nil)
		require.NoError(t, err)
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-Relais-Demo-Token", d.launchToken)
		out, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, tc.code, out.StatusCode)
		_ = out.Body.Close()
	}
}

func TestDefaultPageRemainsHTTP(t *testing.T) {
	listener, fingerprint, err := listenPage(config{HTTP: "127.0.0.1:0"})
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	require.Empty(t, fingerprint)
	_, plainTCP := listener.(*net.TCPListener)
	require.True(t, plainTCP)
}

func TestLocalNetworkHostAllowsOnlySelectedIP(t *testing.T) {
	d := &demo{publicHost: "192.168.1.5"}
	handler := d.handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, host := range []string{"192.168.1.5:9201", "192.168.1.6:9201", "192.168.1.5.attacker.example:9201", "[::ffff:192.168.1.5]:9201", "::ffff:192.168.1.5", "localhost:9201"} {
		req := httptest.NewRequest(http.MethodGet, "https://192.168.1.5:9201/", nil)
		req.Host = host
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		want := http.StatusForbidden
		if host == "192.168.1.5:9201" {
			want = http.StatusNoContent
		}
		require.Equal(t, want, out.Code)
	}
}

func TestLocalNetworkMutationToken(t *testing.T) {
	d := &demo{publicHost: "127.0.0.1", launchToken: "test-launch-token"}
	files := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodHead, http.MethodOptions} {
		for _, token := range []string{"", "wrong", d.launchToken} {
			req := httptest.NewRequest(method, "https://127.0.0.1/unknown", nil)
			req.Header.Set("X-Relais-Demo-Token", token)
			out := httptest.NewRecorder()
			d.handler(files).ServeHTTP(out, req)
			want := http.StatusForbidden
			if token == d.launchToken {
				want = http.StatusNoContent
			}
			require.Equal(t, want, out.Code, "%s token present=%t", method, token != "")
		}
	}
	// Enforce before any route reaches child processes, including curl without Origin.
	for _, target := range []string{"/calls", "/calls/id/move", "/demo/kill", "/demo/drain", "/workers/0/drain"} {
		req := httptest.NewRequest(http.MethodPost, "https://127.0.0.1"+target, nil)
		out := httptest.NewRecorder()
		d.handler(files).ServeHTTP(out, req)
		require.Equal(t, http.StatusForbidden, out.Code, target)
	}
	out := httptest.NewRecorder()
	d.handler(files).ServeHTTP(out, httptest.NewRequest(http.MethodGet, "https://127.0.0.1/", nil))
	require.Equal(t, http.StatusNoContent, out.Code)
	// A correct token does not bypass the existing Origin guard.
	req := httptest.NewRequest(http.MethodPost, "https://127.0.0.1/unknown", nil)
	req.Header.Set("X-Relais-Demo-Token", d.launchToken)
	req.Header.Set("Origin", "https://evil.example")
	out = httptest.NewRecorder()
	d.handler(files).ServeHTTP(out, req)
	require.Equal(t, http.StatusForbidden, out.Code)
	// Missing server token fails closed; loopback-only mode keeps prior behavior.
	d.launchToken = ""
	out = httptest.NewRecorder()
	d.handler(files).ServeHTTP(out, httptest.NewRequest(http.MethodPost, "https://127.0.0.1/unknown", nil))
	require.Equal(t, http.StatusForbidden, out.Code)
	d.publicHost = ""
	out = httptest.NewRecorder()
	d.handler(files).ServeHTTP(out, httptest.NewRequest(http.MethodPost, "http://127.0.0.1/unknown", nil))
	require.Equal(t, http.StatusNoContent, out.Code)
}

func TestLaunchURLAndToken(t *testing.T) {
	token, err := launchToken("127.0.0.1")
	require.NoError(t, err)
	bytes, err := hex.DecodeString(token)
	require.NoError(t, err)
	require.Len(t, bytes, 32)
	next, err := launchToken("127.0.0.1")
	require.NoError(t, err)
	require.NotEqual(t, token, next)
	empty, err := launchToken("")
	require.NoError(t, err)
	require.Empty(t, empty)
	parsed, err := url.Parse(demoPageURL(config{LocalNetwork: "127.0.0.1"}, "127.0.0.1:9201", token))
	require.NoError(t, err)
	require.Equal(t, "https", parsed.Scheme)
	require.Equal(t, "camera", parsed.Query().Get("source"))
	require.Equal(t, "0", parsed.Query().Get("autostart"))
	fragment, err := url.ParseQuery(parsed.Fragment)
	require.NoError(t, err)
	require.Equal(t, token, fragment.Get("token"))
	require.Empty(t, parsed.Query().Get("token")) // not sent in page requests or Referer
	require.Equal(t, "http://localhost:9201/?source=test&autostart=0", demoPageURL(config{}, "127.0.0.1:9201", ""))
}

func TestChromeFingerprint(t *testing.T) {
	cert, fingerprint, err := pageCertificate("127.0.0.1")
	require.NoError(t, err)
	formatted := chromeFingerprint(fingerprint)
	require.Len(t, formatted, 95)
	require.Equal(t, strings.ToUpper(fingerprint), strings.ReplaceAll(formatted, ":", ""))
	sum := sha256.Sum256(cert.Certificate[0])
	require.Equal(t, hex.EncodeToString(sum[:]), fingerprint)
}
