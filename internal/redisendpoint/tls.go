package redisendpoint

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
)

// TLSFromEnv builds verified TLS settings. An empty ServerName lets the Redis
// dialer verify each connection's host, including individual Cluster nodes.
// Error strings name settings but never include their values or file contents.
func TLSFromEnv() (*tls.Config, error) {
	enabled, err := envSwitch("RELAIS_REDIS_TLS")
	if err != nil {
		return nil, err
	}
	ca := os.Getenv("RELAIS_REDIS_TLS_CA_FILE")
	server := os.Getenv("RELAIS_REDIS_TLS_SERVER_NAME")
	cert := os.Getenv("RELAIS_REDIS_TLS_CERT_FILE")
	key := os.Getenv("RELAIS_REDIS_TLS_KEY_FILE")
	if !enabled {
		if ca != "" || server != "" || cert != "" || key != "" {
			return nil, errors.New("Redis TLS settings require RELAIS_REDIS_TLS=1")
		}
		return nil, nil
	}
	if (cert == "") != (key == "") {
		return nil, errors.New("RELAIS_REDIS_TLS_CERT_FILE and RELAIS_REDIS_TLS_KEY_FILE must be set together")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: server}
	if ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, errors.New("cannot read RELAIS_REDIS_TLS_CA_FILE")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("RELAIS_REDIS_TLS_CA_FILE must contain PEM certificates")
		}
		config.RootCAs = roots
	}
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, errors.New("cannot load RELAIS_REDIS_TLS_CERT_FILE and RELAIS_REDIS_TLS_KEY_FILE as a certificate/key pair")
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return config, nil
}
