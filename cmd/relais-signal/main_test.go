package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigAndTokenSources(t *testing.T) {
	c, err := parseConfig([]string{"-listen=127.0.0.1:9443", "-control=http://127.0.0.1:9200", "-advertise-ips=127.0.0.1"})
	require.NoError(t, err)
	_, err = c.validate()
	require.NoError(t, err)
	for _, listen := range []string{"", ":443", "localhost:443", "0.0.0.0:443", "[::]:443", "127.0.0.1:0", "203.0.113.10:443"} {
		invalid := c
		invalid.Listen = listen
		_, err := invalid.validate()
		require.Error(t, err, listen)
	}
	for _, control := range []string{"", "http://public.example", "http://203.0.113.10:9200", "http://127.0.0.1:0", "http://127.0.0.1:", "http://127.0.0.1/path", "http://user:secret@127.0.0.1", "http://127.0.0.1?worker=1", "http://127.0.0.1?", "http://127.0.0.1#fragment", "ftp://127.0.0.1"} {
		invalid := c
		invalid.Control = control
		_, err := invalid.validate()
		require.Error(t, err, control)
	}
	_, err = parseConfig([]string{"-token=secret"})
	require.Error(t, err, "token values cannot be flag arguments")
	_, err = parseConfig([]string{"extra"})
	require.Error(t, err)
	t.Setenv(c.TokenEnv, "")
	_, err = loadToken(c)
	require.Error(t, err)
	t.Setenv(c.TokenEnv, testToken)
	token, err := loadToken(c)
	require.NoError(t, err)
	require.Equal(t, testToken, token)
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("file-token\n"), 0600))
	c.TokenFile = file
	token, err = loadToken(c)
	require.NoError(t, err)
	require.Equal(t, "file-token", token)
	for _, contents := range []string{"", "secret with spaces", "secret\ninjection", strings.Repeat("x", (16<<10)+1)} {
		require.NoError(t, os.WriteFile(file, []byte(contents), 0600))
		_, err := loadToken(c)
		require.Error(t, err)
	}
	require.NoError(t, os.Remove(file))
	_, err = loadToken(c)
	require.Error(t, err)
}

func TestGeneratedAndLoadedCertificates(t *testing.T) {
	c := config{AdvertiseIPs: "127.0.0.1,::1,203.0.113.10"}
	cert, publicPEM, fingerprint, err := certificate(c)
	require.NoError(t, err)
	require.IsType(t, &ecdsa.PrivateKey{}, cert.PrivateKey)
	require.Len(t, cert.Leaf.IPAddresses, 3)
	require.NoError(t, cert.Leaf.VerifyHostname("203.0.113.10"))
	require.Error(t, cert.Leaf.VerifyHostname("203.0.113.11"))
	require.NoError(t, cert.Leaf.CheckSignature(cert.Leaf.SignatureAlgorithm, cert.Leaf.RawTBSCertificate, cert.Leaf.Signature))
	digest := sha256.Sum256(cert.Certificate[0])
	require.Equal(t, hex.EncodeToString(digest[:]), fingerprint)
	again, _, otherFingerprint, err := certificate(c)
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, otherFingerprint)
	require.NotEqual(t, cert.Certificate[0], again.Certificate[0])
	dir := t.TempDir()
	c.CertFile, c.KeyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(c.CertFile, publicPEM, 0600))
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))
	loaded, _, loadedFingerprint, err := certificate(c)
	require.NoError(t, err)
	require.Equal(t, cert.Certificate[0], loaded.Certificate[0])
	require.Equal(t, fingerprint, loadedFingerprint)
	c.AdvertiseIPs = "203.0.113.11"
	_, _, _, err = certificate(c)
	require.Error(t, err)
	for _, ips := range []string{"", "not-an-ip", "0.0.0.0", "224.0.0.1", "fe80::1%en0"} {
		_, _, _, err := certificate(config{AdvertiseIPs: ips})
		require.Error(t, err, ips)
	}
	c.KeyFile = ""
	c.Listen, c.Control = "127.0.0.1:9443", "http://127.0.0.1:9200"
	_, err = c.validate()
	require.Error(t, err)
}

type readyOutput struct{ ready chan string }

func (w readyOutput) Write(data []byte) (int, error) { w.ready <- string(data); return len(data), nil }

func TestRunHTTPSAndShutdown(t *testing.T) {
	otherCert, _, _, err := certificate(config{AdvertiseIPs: "127.0.0.1"})
	require.NoError(t, err)
	otherRoots := x509.NewCertPool()
	otherRoots.AddCert(otherCert.Leaf)
	otherTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: otherRoots}}
	t.Cleanup(otherTransport.CloseIdleConnections)
	client := &http.Client{Transport: otherTransport, Timeout: 5 * time.Second}
	// Select a local port without relying on a common developer-service port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	c := config{Listen: addr, Control: "http://127.0.0.1:9200", AdvertiseIPs: "127.0.0.1", TokenEnv: "RELAIS_SIGNAL_TEST_TOKEN", CertOut: filepath.Join(t.TempDir(), "run-cert.pem")}
	t.Setenv(c.TokenEnv, testToken)
	// Clock does not contact control; a dead private endpoint is sufficient.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan string, 1)
	go func() { done <- run(ctx, c, readyOutput{ready}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(6 * time.Second):
			t.Error("shutdown did not finish")
		}
	})
	select {
	case line := <-ready:
		require.Contains(t, line, "https="+addr)
		require.Contains(t, line, "certificate_sha256=")
		require.NotContains(t, line, testToken)
	case err := <-done:
		t.Fatalf("startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("frontend readiness timeout")
	}
	publicPEM, err := os.ReadFile(c.CertOut)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(publicPEM))
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	trusted := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, _ := request(t, trusted, http.MethodGet, "https://"+addr+"/clock", "", testToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// A different run's trust root must reject this server.
	_, err = client.Get("https://" + addr + "/clock")
	require.Error(t, err)
	// Refuse overwriting a previous run's public trust file.
	secondListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	secondAddr := secondListener.Addr().String()
	require.NoError(t, secondListener.Close())
	err = run(context.Background(), config{Listen: secondAddr, Control: c.Control, AdvertiseIPs: c.AdvertiseIPs, TokenEnv: c.TokenEnv, CertOut: c.CertOut}, readyOutput{ready})
	require.ErrorContains(t, err, "file exists")
}
