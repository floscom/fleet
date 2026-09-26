// Package identity handles fleet's cryptographic identities and pairing.
//
//   - The daemon has a long-lived self-signed TLS certificate (ECDSA P-256,
//     10 years). Its id is the lowercase hex SHA-256 of the cert DER.
//   - Each client device has an Ed25519 key pair. Its device id is the first
//     16 bytes of SHA-256(public key), lowercase hex.
//   - Pairing codes are 8 Crockford-base32 characters (40 bits), displayed as
//     "ABCD-EFGH", single use, with a TTL and at most 5 failed attempts.
//
// See docs/PROTOCOL.md for the exact proof and signature constructions.
package identity

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to a 0600 temp file next to path, syncs it and
// renames it into place, creating the parent directory (0700) if needed.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// exists reports whether path exists, treating errors other than
// "not exist" as existing so callers surface them on read.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
