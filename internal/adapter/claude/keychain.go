package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"strings"
)

// Claude Code on macOS keeps the JSON it writes to .credentials.json on
// Linux in the login Keychain instead: a generic password of the service
// "Claude Code-credentials" and the account of the user (Claude Code
// 2.1.300). With CLAUDE_CONFIG_DIR set, the service name ends in the first 8
// hex digits of the SHA-256 of that directory, so each config dir keeps its
// own login. Claude Code stores it with /usr/bin/security, so reading it
// back the same way does not prompt.

// keychainService is the Keychain service Claude Code keeps its login under.
func keychainService() string {
	const base = "Claude Code-credentials"
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		return base
	}
	sum := sha256.Sum256([]byte(dir))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// readKeychain returns the login stored under service, or an error wrapping
// fs.ErrNotExist when there is none. Replaced by tests.
var readKeychain = func(ctx context.Context, service string) ([]byte, error) {
	args := []string{"find-generic-password", "-s", service, "-w"}
	if u, err := osuser.Current(); err == nil {
		args = append(args, "-a", u.Username)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/security", args...).Output()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 44: // errSecItemNotFound
		return nil, fmt.Errorf("keychain item %q: %w", service, fs.ErrNotExist)
	case errors.As(err, &ee):
		return nil, fmt.Errorf("reading the login from the Keychain: %s", strings.TrimSpace(string(ee.Stderr)))
	case err != nil:
		return nil, fmt.Errorf("reading the login from the Keychain: %w", err)
	}
	return out, nil
}
