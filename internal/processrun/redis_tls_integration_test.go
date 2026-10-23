package processrun

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/relaishq/relais/pkg/util/bootstrap"
	"github.com/stretchr/testify/require"
)

// Certificates are generated per test. No provider credentials or external
// Redis instance are used, and neither the server nor the client uses 6379.
func redisTLSCertificates(t *testing.T, dir string) (caFile, certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Relais test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	server := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Redis test server"}, DNSNames: []string{"localhost", "redis.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	private, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile, certFile, keyFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	require.NoError(t, os.WriteFile(caFile, caPEM, 0600))
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600))
	roots = x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))
	return
}

func redisTLSUnavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("RELAIS_TEST_REDIS_TLS_REQUIRE") == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}

func TestRedisTLSACLIntegration(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		redisTLSUnavailable(t, "Redis TLS integration: redis-server is missing from PATH")
		return
	}
	dir := t.TempDir()
	ca, cert, key, roots := redisTLSCertificates(t, dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NoError(t, redisendpoint.ValidateDev(addr))
	require.NoError(t, listener.Close())
	logFile, err := os.Create(filepath.Join(dir, "redis.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	// Start with ordinary server-authenticated TLS, then require mTLS below.
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", "0", "--tls-port", port, "--tls-cert-file", cert, "--tls-key-file", key, "--tls-ca-cert-file", ca, "--tls-auth-clients", "no", "--save", "", "--appendonly", "no", "--dir", dir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	var processErr error
	go func() { processErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		<-done
	})
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	admin := redis.NewClient(&redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, Addr: addr, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{pair}}, DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = admin.Close() })
	startup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if err = admin.Ping(startup).Err(); err == nil {
			break
		}
		select {
		case <-done:
			output, readErr := os.ReadFile(logFile.Name())
			require.NoError(t, readErr)
			text := string(output)
			if strings.Contains(text, "TLS support not compiled") || strings.Contains(text, "TLS Support is not compiled") || (strings.Contains(text, "tls-port") && strings.Contains(text, "Bad directive")) {
				redisTLSUnavailable(t, "Redis TLS integration: redis-server lacks TLS support: "+text)
				return
			}
			t.Fatalf("throwaway TLS Redis exited: %v; %s", processErr, text)
		case <-startup.Done():
			t.Fatalf("throwaway TLS Redis did not become ready: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	username, password := "relais-"+rand.Text(), rand.Text()
	require.NoError(t, admin.Do(startup, "ACL", "SETUSER", username, "on", ">"+password, "~*", "+@all").Err())
	require.NoError(t, admin.Do(startup, "ACL", "SETUSER", "default", "off").Err())
	require.NoError(t, admin.Close())
	control := redis.NewClient(&redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, Addr: addr, Username: username, Password: password, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{pair}}, MaxRetries: -1})
	t.Cleanup(func() { _ = control.Close() })
	wrongCA, _, _, _ := redisTLSCertificates(t, t.TempDir())
	cases := []struct {
		name, ca, user, tls, server, cert, key string
		wantError, requireClientCert           bool
	}{
		{name: "TLS and ACL", ca: ca, user: username, tls: "1"},
		{name: "mutual TLS", ca: ca, user: username, tls: "1", cert: cert, key: key, requireClientCert: true},
		{name: "server name override", ca: ca, user: username, tls: "1", server: "redis.test", cert: cert, key: key},
		{name: "wrong server name", ca: ca, user: username, tls: "1", server: "wrong.test", cert: cert, key: key, wantError: true},
		{name: "wrong CA", ca: wrongCA, user: username, tls: "1", cert: cert, key: key, wantError: true},
		{name: "wrong user", ca: ca, user: "wrong-" + username, tls: "1", cert: cert, key: key, wantError: true},
		{name: "plaintext", user: username, wantError: true},
		{name: "missing client certificate", ca: ca, user: username, tls: "1", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.requireClientCert {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				require.NoError(t, control.ConfigSet(ctx, "tls-auth-clients", "yes").Err())
			}
			t.Setenv("RELAIS_REDIS_TLS", tc.tls)
			t.Setenv("RELAIS_REDIS_TLS_CA_FILE", tc.ca)
			t.Setenv("RELAIS_REDIS_TLS_SERVER_NAME", tc.server)
			t.Setenv("RELAIS_REDIS_TLS_CERT_FILE", tc.cert)
			t.Setenv("RELAIS_REDIS_TLS_KEY_FILE", tc.key)
			t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
			t.Setenv("RELAIS_REDIS_ADDR", addr)
			t.Setenv("RELAIS_REDIS_USERNAME", tc.user)
			t.Setenv("RELAIS_REDIS_PASSWORD", password)
			t.Setenv("RELAIS_REDIS_PREFIX", "tls-test:")
			var config StoreConfig
			config.ConnectionFlags(flag.NewFlagSet("tls-redis", flag.ContinueOnError))
			for _, kind := range []string{"session", "frames", "owners", "storage", "bootstrap"} {
				t.Run(kind, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					var closeClient func() error
					var err error
					switch kind {
					case "session":
						client, e := config.Open(ctx)
						err = e
						if client != nil {
							closeClient = client.Close
						}
					case "frames":
						client, e := config.OpenFrames(ctx)
						err = e
						if client != nil {
							closeClient = client.Close
						}
					case "owners":
						client, e := config.OpenOwners(ctx)
						err = e
						if client != nil {
							closeClient = client.Close
						}
					case "bootstrap":
						t.Setenv("RELAIS_STORAGE_TYPE", "redis")
						t.Setenv("RELAIS_STORAGE_REDIS_URL", addr)
						t.Setenv("RELAIS_STORAGE_REDIS_PASSWORD", password)
						// Production process settings must not switch the legacy endpoint or password.
						t.Setenv("RELAIS_REDIS_ADDR", "must-not-dial.example.invalid:1")
						t.Setenv("RELAIS_REDIS_PASSWORD", "")
						_, client, _, e := bootstrap.Init(ctx, "tls-test", "test")
						err = e
						if client != nil {
							closeClient = client.Close
						}
					case "storage":
						cfg, e := config.redisConfig()
						require.NoError(t, e)
						client, e := storage.NewRedisStorage(cfg)
						err = e
						if client != nil {
							closeClient = client.Close
						}
					}
					if closeClient != nil {
						defer func() { require.NoError(t, closeClient()) }()
					}
					if tc.wantError {
						require.Error(t, err)
						require.NotContains(t, err.Error(), username)
						require.NotContains(t, err.Error(), password)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}
