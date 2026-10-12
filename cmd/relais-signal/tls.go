package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"time"
)

func advertisedIPs(value string) ([]net.IP, error) {
	if value == "" {
		return nil, nil
	}
	var ips []net.IP
	for _, item := range strings.Split(value, ",") {
		ip, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			return nil, errors.New("-advertise-ips requires specific literal IP addresses")
		}
		ips = append(ips, net.IP(ip.AsSlice()))
	}
	return ips, nil
}

func certificate(c config) (tls.Certificate, []byte, string, error) {
	ips, err := advertisedIPs(c.AdvertiseIPs)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	var cert tls.Certificate
	var publicPEM []byte
	if c.CertFile != "" {
		cert, err = tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	} else {
		if len(ips) == 0 {
			return cert, nil, "", errors.New("generated certificate requires -advertise-ips")
		}
		var key *ecdsa.PrivateKey
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return cert, nil, "", err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return cert, nil, "", err
		}
		now := time.Now()
		template := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: "relais-signal test frontend"},
			NotBefore:    now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			IPAddresses:           ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			return cert, nil, "", err
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
		publicPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	if err != nil {
		return cert, nil, "", err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil, "", err
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return cert, nil, "", errors.New("certificate is outside its validity period")
	}
	for _, ip := range ips {
		if err := leaf.VerifyHostname(ip.String()); err != nil {
			return cert, nil, "", errors.New("certificate lacks a configured advertised IP SAN")
		}
	}
	cert.Leaf = leaf
	digest := sha256.Sum256(leaf.Raw)
	return cert, publicPEM, hex.EncodeToString(digest[:]), nil
}
