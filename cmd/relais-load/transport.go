package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// callerHTTPClient confines the optional token and certificate to the
// signaling origin. TLS still verifies the chain, validity and URL's IP SAN
// or hostname before checking the exact pinned server certificate.
func callerHTTPClient(signaling, certFile, tokenFile string) (*http.Client, error) {
	u, err := url.Parse(signaling)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || !strings.HasSuffix(u.Path, "/calls") {
		return nil, errors.New("signaling must be an HTTP(S) calls URL without credentials, query or fragment")
	}
	if certFile == "" && tokenFile == "" {
		return nil, nil // Preserve the existing local and owned-child defaults.
	}
	if u.Scheme != "https" {
		return nil, errors.New("signaling certificate and bearer token require HTTPS")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if certFile != "" {
		data, err := os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("read signaling certificate: %w", err)
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("signaling certificate must contain one PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse signaling certificate: %w", err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, cert.Raw) {
					return errors.New("signaling certificate does not match the pin")
				}
				return nil
			},
		}
	}
	var token string
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read bearer token: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, " \t\r\n") {
			return nil, errors.New("bearer token file must contain one nonempty token")
		}
	}
	return &http.Client{
		Transport: &callerTransport{base: transport, origin: u, token: token},
		Timeout:   10 * time.Second,
	}, nil
}

type callerTransport struct {
	base   *http.Transport
	origin *url.URL
	token  string
}

func (t *callerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || !strings.EqualFold(req.URL.Host, t.origin.Host) {
		return nil, errors.New("caller HTTP request leaves the signaling origin")
	}
	cloned := req.Clone(req.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(cloned)
}

func (t *callerTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
