package redisendpoint

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func clearTLS(t *testing.T) {
	t.Helper()
	for _, name := range []string{"RELAIS_REDIS_TLS", "RELAIS_REDIS_TLS_CA_FILE", "RELAIS_REDIS_TLS_SERVER_NAME", "RELAIS_REDIS_TLS_CERT_FILE", "RELAIS_REDIS_TLS_KEY_FILE"} {
		t.Setenv(name, "")
	}
}

func testCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	private, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600))
	return certFile, keyFile
}

func TestTLSVariables(t *testing.T) {
	clearTLS(t)
	config, err := TLSFromEnv()
	require.NoError(t, err)
	require.Nil(t, config)
	t.Setenv("RELAIS_REDIS_TLS", "0")
	config, err = TLSFromEnv()
	require.NoError(t, err)
	require.Nil(t, config)
	t.Setenv("RELAIS_REDIS_TLS", "1")
	config, err = TLSFromEnv()
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), config.MinVersion)
	require.False(t, config.InsecureSkipVerify)
	require.Nil(t, config.RootCAs, "nil selects system roots")
	require.Empty(t, config.ServerName, "the dialer defaults verification to each host")
	require.Empty(t, config.Certificates)
	cert, key := testCertificate(t)
	t.Setenv("RELAIS_REDIS_TLS_CA_FILE", cert)
	t.Setenv("RELAIS_REDIS_TLS_SERVER_NAME", "cache.example.invalid")
	t.Setenv("RELAIS_REDIS_TLS_CERT_FILE", cert)
	t.Setenv("RELAIS_REDIS_TLS_KEY_FILE", key)
	config, err = TLSFromEnv()
	require.NoError(t, err)
	require.NotNil(t, config.RootCAs)
	require.Equal(t, "cache.example.invalid", config.ServerName)
	require.Len(t, config.Certificates, 1)
	require.False(t, config.InsecureSkipVerify)
}

func TestInvalidTLSVariablesDoNotLeakSecrets(t *testing.T) {
	cert, key := testCertificate(t)
	_, otherKey := testCertificate(t)
	secret := "credential-and-key-material-must-stay-private"
	invalid := filepath.Join(t.TempDir(), secret)
	require.NoError(t, os.WriteFile(invalid, []byte(secret), 0600))
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"bad switch", map[string]string{"RELAIS_REDIS_TLS": secret}, "RELAIS_REDIS_TLS"},
		{"missing CA", map[string]string{"RELAIS_REDIS_TLS_CA_FILE": invalid + "-missing"}, "RELAIS_REDIS_TLS_CA_FILE"},
		{"invalid CA", map[string]string{"RELAIS_REDIS_TLS_CA_FILE": invalid}, "PEM"},
		{"half cert", map[string]string{"RELAIS_REDIS_TLS_CERT_FILE": cert}, "set together"},
		{"half key", map[string]string{"RELAIS_REDIS_TLS_KEY_FILE": key}, "set together"},
		{"missing cert", map[string]string{"RELAIS_REDIS_TLS_CERT_FILE": invalid + "-missing", "RELAIS_REDIS_TLS_KEY_FILE": key}, "certificate/key pair"},
		{"missing key", map[string]string{"RELAIS_REDIS_TLS_CERT_FILE": cert, "RELAIS_REDIS_TLS_KEY_FILE": invalid + "-missing"}, "certificate/key pair"},
		{"bad pair", map[string]string{"RELAIS_REDIS_TLS_CERT_FILE": cert, "RELAIS_REDIS_TLS_KEY_FILE": invalid}, "certificate/key pair"},
		{"mismatched pair", map[string]string{"RELAIS_REDIS_TLS_CERT_FILE": cert, "RELAIS_REDIS_TLS_KEY_FILE": otherKey}, "certificate/key pair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTLS(t)
			t.Setenv("RELAIS_REDIS_TLS", "1")
			t.Setenv("RELAIS_REDIS_USERNAME", secret)
			t.Setenv("RELAIS_REDIS_PASSWORD", secret)
			for name, value := range tc.vars {
				t.Setenv(name, value)
			}
			config, err := TLSFromEnv()
			require.Nil(t, config)
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), secret)
		})
	}
	for _, name := range []string{"RELAIS_REDIS_TLS_CA_FILE", "RELAIS_REDIS_TLS_SERVER_NAME", "RELAIS_REDIS_TLS_CERT_FILE", "RELAIS_REDIS_TLS_KEY_FILE"} {
		for _, toggle := range []string{"", "0"} {
			t.Run(name+toggle, func(t *testing.T) {
				clearTLS(t)
				t.Setenv("RELAIS_REDIS_TLS", toggle)
				t.Setenv(name, secret)
				_, err := TLSFromEnv()
				require.ErrorContains(t, err, "RELAIS_REDIS_TLS=1")
				require.False(t, strings.Contains(err.Error(), secret))
			})
		}
	}
}
