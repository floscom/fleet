package claude

import (
	"context"
	"os"
	"path/filepath"
)

// Claude Code on Linux keeps its login in <config dir>/.credentials.json
// (mode 0600) and replaces it atomically while holding the proper-lockfile
// lock "<config dir>/.storage-write.lock" (Claude Code 2.1.280). On macOS it
// uses the Keychain instead and there is no file.

// AuthFile implements adapter.AuthProvider.
func (c *claude) AuthFile(home string) string {
	if home != "" {
		return filepath.Join(home, ".claude", ".credentials.json")
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(h, ".claude")
	}
	return filepath.Join(dir, ".credentials.json")
}

// LockAuth implements adapter.AuthProvider.
func (c *claude) LockAuth(ctx context.Context, file string) (func(), error) {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return lockFile(ctx, filepath.Join(dir, ".storage-write.lock"))
}
