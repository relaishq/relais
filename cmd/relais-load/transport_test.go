package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/loadgen"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func testCallerCertificate(t *testing.T, ip string) (tls.Certificate, string) {
	t.Helper()
	return testSignedCallerCertificate(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "caller test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP(ip)},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}, nil, nil)
}

func testSignedCallerCertificate(t *testing.T, cert, parent *x509.Certificate, signingKey *ecdsa.PrivateKey) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	cert.SerialNumber = serial
	if parent == nil {
		parent = cert
	}
	if signingKey == nil {
		signingKey = key
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, parent, &key.PublicKey, signingKey)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "server.pem")
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, file
}

// The helper process runs the public CLI path, including RunChild's poller,
// while keeping process signals, stdout and GOMAXPROCS out of the test host.
func TestExternalCallerProcess(_ *testing.T) {
	if os.Getenv("RELAIS_EXTERNAL_CALLER_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if err := runArgs(os.Args[i+1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestExternalCallerHTTPS(t *testing.T) {
	system, err := callharness.Start(callharness.Options{Relay: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = system.Close() })
	target, err := url.Parse(strings.TrimSuffix(system.SignalingURL(), "/calls"))
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	cert, certFile := testCallerCertificate(t, "127.0.0.1")
	_, wrongFile := testCallerCertificate(t, "127.0.0.1")
	badSAN, badSANFile := testCallerCertificate(t, "127.0.0.2")
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("test-caller-token\n"), 0600))
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, pin, token, failure string
		certificate               tls.Certificate
	}{
		{name: "pinned authenticated caller", pin: certFile, token: tokenFile, certificate: cert},
		{name: "wrong pin", pin: wrongFile, token: tokenFile, certificate: cert, failure: "certificate signed by unknown authority"},
		{name: "no pin", token: tokenFile, certificate: cert, failure: "certificate signed by unknown authority"},
		{name: "no token", pin: certFile, certificate: cert, failure: "401 Unauthorized"},
		{name: "wrong IP SAN", pin: badSANFile, token: tokenFile, certificate: badSAN, failure: "not 127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			requests := map[string]int{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-caller-token" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				mu.Lock()
				requests[r.Method+" "+r.URL.Path]++
				mu.Unlock()
				proxy.ServeHTTP(w, r)
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{tc.certificate}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			t.Cleanup(server.Close)
			output := filepath.Join(t.TempDir(), "caller.jsonl")
			args := []string{"-test.run=^TestExternalCallerProcess$", "--", "-external-caller", "-signaling=" + server.URL + "/calls", "-relay=" + system.RelayAddr().String(), "-caller-bind=127.0.0.1:0", "-callers=1", "-duration=2s", "-sample-every=1s", "-video=false", "-output=" + output}
			if tc.pin != "" {
				args = append(args, "-signaling-cert="+tc.pin)
			}
			if tc.token != "" {
				args = append(args, "-bearer-token-file="+tc.token)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, args...)
			cmd.Env = append(os.Environ(), "RELAIS_EXTERNAL_CALLER_TEST=1")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var ready bool
			var capturedStdout bytes.Buffer
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				capturedStdout.Write(scanner.Bytes())
				capturedStdout.WriteByte('\n')
				var message struct {
					Type string `json:"type"`
					HTTP string `json:"http"`
				}
				if json.Unmarshal(scanner.Bytes(), &message) == nil && message.Type == "ready" {
					ready = true
					body := strings.NewReader(fmt.Sprintf(`{"at":%q}`, time.Now().UTC().Format(time.RFC3339Nano)))
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, message.HTTP+"/start", body)
					require.NoError(t, err)
					response, err := http.DefaultClient.Do(req)
					require.NoError(t, err)
					_ = response.Body.Close()
					require.Equal(t, http.StatusOK, response.StatusCode)
				}
			}
			waitErr := cmd.Wait()
			require.NoError(t, scanner.Err())
			require.NotContains(t, capturedStdout.String(), "test-caller-token", "token disclosed on stdout")
			require.NotContains(t, stderr.String(), "test-caller-token", "token disclosed on stderr")
			data, err := os.ReadFile(output)
			require.NoError(t, err)
			require.NotContains(t, string(data), "test-caller-token", "token disclosed in output file")
			if tc.failure != "" {
				require.Error(t, waitErr)
				require.Contains(t, stderr.String(), tc.failure)
				require.False(t, ready)
				return
			}
			require.NoError(t, waitErr, stderr.String())
			require.True(t, ready)
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, 1, requests["POST /calls"])
			require.GreaterOrEqual(t, requests["GET /status"], 2, "periodic and final status must use the injected client")
			var deletes int
			for key, count := range requests {
				if strings.HasPrefix(key, "DELETE /calls/") {
					deletes += count
				}
			}
			require.Equal(t, 1, deletes)
			var calls int
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				var message loadgen.Message
				require.NoError(t, json.Unmarshal(line, &message))
				if message.Type == "call" {
					calls++
					require.Empty(t, message.Error)
					require.NotNil(t, message.Call)
					require.NotEmpty(t, message.Call.Tracks, "real echoed media")
				}
			}
			require.Equal(t, 1, calls)
		})
	}
}

func TestCallerCertificateExactPin(t *testing.T) {
	ca, pinFile := testSignedCallerCertificate(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "test pin CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}, nil, nil)
	parent, err := x509.ParseCertificate(ca.Certificate[0])
	require.NoError(t, err)
	leaf, _ := testSignedCallerCertificate(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "different server leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, parent, ca.PrivateKey.(*ecdsa.PrivateKey))
	parsedLeaf, err := x509.ParseCertificate(leaf.Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(parent)
	_, err = parsedLeaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"})
	require.NoError(t, err, "normal chain and IP SAN verification must accept this leaf")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := callerHTTPClient(server.URL+"/calls", pinFile, "")
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL + "/calls")
	if response != nil {
		_ = response.Body.Close()
	}
	require.ErrorContains(t, err, "does not match the pin")
}

func TestCallerCertificateValidityAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name                string
		notBefore, notAfter time.Time
	}{
		{"expired", time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour)},
		{"not yet valid", time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pin := testSignedCallerCertificate(t, &x509.Certificate{NotBefore: tc.notBefore, NotAfter: tc.notAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature}, nil, nil)
			_, err := callerHTTPClient("https://127.0.0.1/calls", pin, "")
			require.ErrorContains(t, err, "outside its validity dates")
		})
	}
}

func TestCallerTokenFilePermissionsWarning(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0640, 0604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(tokenFile, []byte(" \ttest-private-token\r\n"), mode))
			info, err := os.Stat(tokenFile)
			require.NoError(t, err)
			warningFile := filepath.Join(t.TempDir(), "warnings")
			warnings, err := os.Create(warningFile)
			require.NoError(t, err)
			t.Cleanup(func() { _ = warnings.Close() })
			client, err := func() (*http.Client, error) {
				previous := os.Stderr
				os.Stderr = warnings
				defer func() { os.Stderr = previous }()
				return callerHTTPClient("https://127.0.0.1/calls", "", tokenFile)
			}()
			require.NoError(t, err, "file permissions only warn")
			defer client.CloseIdleConnections()
			require.Equal(t, "test-private-token", client.Transport.(*callerTransport).token)
			data, err := os.ReadFile(warningFile)
			require.NoError(t, err)
			require.NotContains(t, string(data), "test-private-token")
			if info.Mode().Perm()&0044 != 0 {
				require.Contains(t, string(data), "group- or world-readable")
			} else {
				require.Empty(t, data)
			}
		})
	}
}

func TestCallerTransportRejectsOtherOrigins(t *testing.T) {
	cert, file := testCallerCertificate(t, "127.0.0.1")
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("test-token"), 0600))
	var received atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Store(true) }))
	defer other.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := callerHTTPClient(server.URL+"/calls", file, token)
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	_, err = client.Get(server.URL + "/calls")
	require.ErrorContains(t, err, "leaves the signaling origin")
	_, err = client.Get(other.URL + "/calls/other")
	require.ErrorContains(t, err, "leaves the signaling origin")
	body := &callerRequestBody{Reader: strings.NewReader("request content")}
	req, err := http.NewRequest(http.MethodPost, other.URL+"/calls", body)
	require.NoError(t, err)
	_, err = client.Transport.RoundTrip(req)
	require.ErrorContains(t, err, "leaves the signaling origin")
	require.True(t, body.closed, "RoundTrip must close the body when it rejects a request")
	require.False(t, received.Load(), "redirects and resource URLs cannot disclose the token")
}

type callerRequestBody struct {
	io.Reader
	closed bool
}

func (b *callerRequestBody) Close() error {
	b.closed = true
	return nil
}

func TestExternalCallerFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-signaling=https://127.0.0.1/calls"},
		{"-external-caller", "-child"},
		{"-external-caller", "-output=unused", "-event=10s,move,1,1"},
		{"-external-caller", "-output=unused", "-processes=2"},
		{"-external-caller", "-output=unused", "-capacity"},
		{"-external-caller"},
		{"-external-caller", "-output=unused", "-relay=[::1]:9000"},
	} {
		require.Error(t, runArgs(args), "args=%v", args)
	}
	for _, signaling := range []string{"https://user:pass@127.0.0.1/calls", "https://127.0.0.1/status", "https://127.0.0.1/calls?token=x", "https://127.0.0.1/calls#token"} {
		_, err := callerHTTPClient(signaling, "", "")
		require.Error(t, err)
	}
	_, err := callerHTTPClient("http://127.0.0.1/calls", "", "token")
	require.ErrorContains(t, err, "require HTTPS")
}
