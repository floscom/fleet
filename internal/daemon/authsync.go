package daemon

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/config"
)

// authSyncInterval is how often credentials are synced while sandboxed
// agents run ([sandbox] auth).
const authSyncInterval = 5 * time.Second

// Sharing the host login with sandboxes ([sandbox] auth) cannot use a bind
// mount: the CLIs replace their credentials file atomically (a rename onto a
// mounted file fails), and a snap-packaged Docker cannot see ~/.claude or
// ~/.codex at all. Copying once is not enough either: OAuth refresh tokens
// rotate, so whichever side refreshes first would log the other one out.
// Instead the file is kept in sync both ways: the newer copy wins, and is
// written under the CLI's own write lock. To the CLIs this looks like two
// sessions on one machine sharing a login.

// syncAuth syncs the credentials of every adapter in [sandbox] auth between
// the daemon user's home and the sandbox home.
func (m *manager) syncAuth(ctx context.Context, cfg config.Config) {
	if len(cfg.Sandbox.Auth) == 0 {
		return
	}
	home := filepath.Join(cfg.Sandbox.DirOrDefault(), "home")
	for _, id := range cfg.Sandbox.Auth {
		ad, _ := m.d.opts.Adapters.Get(id)
		ap, ok := ad.(adapter.AuthProvider)
		if !ok {
			continue // reported at startup
		}
		host := ap.AuthFile("")
		if host == "" {
			continue
		}
		if err := syncFile(ctx, host, ap.AuthFile(home), ap.LockAuth); err != nil {
			m.d.log.Warn("syncing login into the sandbox failed", "adapter", id, "err", err)
		}
	}
}

// checkAuthConfig logs [sandbox] auth entries that cannot be synced.
func (d *daemon) checkAuthConfig(cfg config.Config) {
	for _, id := range cfg.Sandbox.Auth {
		ad, ok := d.opts.Adapters.Get(id)
		if !ok {
			d.log.Warn("[sandbox] auth names an unknown adapter", "adapter", id)
		} else if _, ok := ad.(adapter.AuthProvider); !ok {
			d.log.Warn("[sandbox] auth: adapter keeps no login file", "adapter", id)
		}
	}
}

type lockFunc func(ctx context.Context, file string) (unlock func(), err error)

// syncFile makes the sandbox copy of a credentials file match the host's,
// in both directions: the file with the newer mtime is copied over the other
// (keeping its mtime, so an unchanged pair is recognized by stat alone). A
// missing sandbox copy is created; a missing host file is never created, so
// logging out on the host is not undone by a sandbox.
func syncFile(ctx context.Context, host, sandbox string, lock lockFunc) error {
	hi, err := os.Stat(host)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	si, err := os.Stat(sandbox)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return copyAuth(ctx, host, sandbox, lock)
	case err != nil:
		return err
	case si.ModTime().Equal(hi.ModTime()):
		return nil
	case si.ModTime().After(hi.ModTime()):
		return copyAuth(ctx, sandbox, host, lock)
	default:
		return copyAuth(ctx, host, sandbox, lock)
	}
}

// copyAuth replaces dst with src (mode 0600, src's mtime) under dst's lock.
func copyAuth(ctx context.Context, src, dst string, lock lockFunc) error {
	unlock, err := lock(ctx, dst)
	if err != nil {
		return err
	}
	defer unlock()
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Replace the target of a symlinked file (dotfile managers), not the link.
	if real, err := filepath.EvalSymlinks(dst); err == nil {
		dst = real
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
		return os.Chtimes(dst, si.ModTime(), si.ModTime())
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".fleet-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp, si.ModTime(), si.ModTime())
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
