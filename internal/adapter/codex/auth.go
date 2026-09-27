package codex

import (
	"context"
	"os"
	"path/filepath"
)

// Codex keeps its login in $CODEX_HOME/auth.json (default ~/.codex), unless
// cli_auth_credentials_store puts it in the OS keyring. It takes no lock.

// AuthFile implements adapter.AuthProvider.
func (c *codex) AuthFile(home string) string {
	if home != "" {
		return filepath.Join(home, ".codex", "auth.json")
	}
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(h, ".codex")
	}
	return filepath.Join(dir, "auth.json")
}

// LockAuth implements adapter.AuthProvider; Codex uses no lock.
func (c *codex) LockAuth(context.Context, string) (func(), error) { return func() {}, nil }
