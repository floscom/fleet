package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// Device is a client device's key pair.
type Device struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	ID      string
}

// LoadOrCreateDevice loads identity/device.key under dir, creating it (0600).
func LoadOrCreateDevice(dir string) (*Device, error) {
	path := filepath.Join(dir, "identity", "device.key")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return nil, fmt.Errorf("save device key: %w", err)
		}
		return newDevice(priv), nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not a PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return newDevice(priv), nil
}

func newDevice(priv ed25519.PrivateKey) *Device {
	pub := priv.Public().(ed25519.PublicKey)
	return &Device{Private: priv, Public: pub, ID: DeviceID(pub)}
}

// DeviceID derives the device id from a public key.
func DeviceID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// AuthMessage is the byte string a device signs to authenticate:
// "fleet-auth-v1" || nonce || serverFP.
func AuthMessage(nonce, serverFP []byte) []byte {
	return concat([]byte("fleet-auth-v1"), nonce, serverFP)
}

// Sign signs AuthMessage(nonce, serverFP).
func (d *Device) Sign(nonce, serverFP []byte) []byte {
	return ed25519.Sign(d.Private, AuthMessage(nonce, serverFP))
}

// VerifyAuth verifies a device signature.
func VerifyAuth(pub ed25519.PublicKey, nonce, serverFP, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, AuthMessage(nonce, serverFP), sig)
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
