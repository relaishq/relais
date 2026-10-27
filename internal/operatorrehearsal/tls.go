package operatorrehearsal

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func writeFiles(dir string, files map[string][]byte) error {
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			return err
		}
	}
	return nil
}
func certificates(dir string) ([]byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local rehearsal CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for i, names := range [][]string{{StoreEndpoint, StoreNode}, {SignalName}} {
		key, x := rsa.GenerateKey(rand.Reader, 2048)
		if x != nil {
			return nil, x
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
		if i == 1 {
			cert.IPAddresses = []net.IP{net.ParseIP(SignalIP)}
		}
		b, x := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if x != nil {
			return nil, x
		}
		public := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})
		private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		name := "store"
		if i == 1 {
			name = "signal"
		}
		if x = writeFiles(filepath.Join(dir, name), map[string][]byte{"ca.crt": caPEM, "server.crt": public, "server.key": private, "server.pem": append(public, private...)}); x != nil {
			return nil, x
		}
		// Disposable test keys: container service users need traverse/read access.
		if x = os.Chmod(filepath.Join(dir, name), 0755); x != nil {
			return nil, x
		}
		for _, f := range []string{"ca.crt", "server.crt", "server.key", "server.pem"} {
			if x = os.Chmod(filepath.Join(dir, name, f), 0644); x != nil {
				return nil, x
			}
		}
	}
	return caPEM, nil
}
