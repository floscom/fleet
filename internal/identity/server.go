package identity

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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Server is the daemon's TLS identity.
type Server struct {
	Cert tls.Certificate
	// ID is hex SHA-256 of Cert.Certificate[0].
	ID string
	// Fingerprint is the raw 32-byte SHA-256 of the cert DER.
	Fingerprint []byte
}

// LoadOrCreateServer loads identity/server.{key,crt} under dir, creating them
// (0600) on first use.
func LoadOrCreateServer(dir, commonName string) (*Server, error) {
	keyPath := filepath.Join(dir, "identity", "server.key")
	crtPath := filepath.Join(dir, "identity", "server.crt")
	haveKey, haveCrt := exists(keyPath), exists(crtPath)
	switch {
	case haveKey && haveCrt:
	case !haveKey && !haveCrt:
		if err := createServer(keyPath, crtPath, commonName); err != nil {
			return nil, fmt.Errorf("create server identity: %w", err)
		}
	default:
		// Never silently replace half an identity: paired devices pin it.
		return nil, fmt.Errorf("incomplete server identity in %s (need both server.key and server.crt)", filepath.Dir(keyPath))
	}
	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server identity: %w", err)
	}
	fp := CertFingerprint(cert.Certificate[0])
	return &Server{Cert: cert, ID: hex.EncodeToString(fp), Fingerprint: fp}, nil
}

func createServer(keyPath, crtPath, commonName string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	if commonName == "" {
		commonName = "fleet"
	}
	dns := []string{"localhost"}
	if hn, err := os.Hostname(); err == nil && hn != "" && hn != "localhost" {
		dns = append(dns, hn)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"fleet"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	if err := writeFileAtomic(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return errors.Join(err, os.Remove(keyPath))
	}
	return nil
}

// TLSConfig returns the server-side TLS config (TLS 1.3 min, no client certs;
// devices authenticate at the application layer).
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.Cert},
		ClientAuth:   tls.NoClientCert,
	}
}

// CertFingerprint returns SHA-256 of a DER certificate.
func CertFingerprint(der []byte) []byte {
	sum := sha256.Sum256(der)
	return sum[:]
}
