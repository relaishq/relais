package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"time"

	"github.com/relais/internal/processrun"
)

// No wildcard, DNS resolution, or public address can broaden this opt-in.
// The bind itself also checks that the chosen address belongs to this host.
func validateLocalAddress(host string) error {
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return errors.New("local-network must be a literal loopback or private IPv4 address; IPv6 media is not supported by this demo")
	}
	return nil
}

func pageAddress(cfg config) string {
	if cfg.LocalNetwork == "" {
		return cfg.HTTP
	}
	_, port, _ := net.SplitHostPort(cfg.HTTP)
	return net.JoinHostPort(cfg.LocalNetwork, port)
}

func relayArgs(cfg config) []string {
	host := cfg.LocalNetwork
	if host == "" {
		host = "127.0.0.1"
	}
	return []string{"-media", net.JoinHostPort(host, "0"), "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0"}
}

// The key and certificate live only in memory and are new on each startup.
func pageCertificate(host string) (tls.Certificate, string, error) {
	if err := validateLocalAddress(host); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:]), nil
}

func listenPage(cfg config) (net.Listener, string, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, "", err
	}
	if cfg.LocalNetwork == "" {
		listener, err := processrun.ListenPrivate(cfg.HTTP)
		return listener, "", err
	}
	cert, fingerprint, err := pageCertificate(cfg.LocalNetwork)
	if err != nil {
		return nil, "", err
	}
	listener, err := net.Listen("tcp", pageAddress(cfg))
	if err != nil {
		return nil, "", err
	}
	return tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}), fingerprint, nil
}
