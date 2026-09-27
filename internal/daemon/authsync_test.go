package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	host := filepath.Join(dir, "host", ".credentials.json")
	sb := filepath.Join(dir, "sandbox", "home", ".claude", ".credentials.json")
	var locked []string
	lock := func(_ context.Context, file string) (func(), error) {
		locked = append(locked, file)
		return func() {}, nil
	}
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	write := func(p, s string, mtime time.Time) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	// No host login: nothing happens, in particular no host file appears.
	write(sb, "sandbox-only", t0)
	if err := syncFile(ctx, host, sb, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(host); !os.IsNotExist(err) {
		t.Fatal("host credentials were created from the sandbox")
	}
	os.Remove(sb)

	// Host -> missing sandbox copy, with mode 0600 and the host's mtime.
	write(host, "v1", t0)
	if err := syncFile(ctx, host, sb, lock); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(sb)
	if read(sb) != "v1" || fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(t0) {
		t.Fatalf("sandbox copy %q mode %v mtime %v", read(sb), fi.Mode(), fi.ModTime())
	}
	if len(locked) != 1 || locked[0] != sb {
		t.Errorf("locked %q, want the sandbox file", locked)
	}

	// Unchanged: no copy, no lock.
	locked = nil
	if err := syncFile(ctx, host, sb, lock); err != nil || len(locked) != 0 {
		t.Fatalf("unchanged pair: err %v, locked %q", err, locked)
	}

	// The sandbox refreshed its token: the newer copy goes back to the host.
	write(sb, "v2", t0.Add(time.Minute))
	if err := syncFile(ctx, host, sb, lock); err != nil {
		t.Fatal(err)
	}
	if read(host) != "v2" || locked[0] != host {
		t.Fatalf("host = %q, locked %q", read(host), locked)
	}

	// And the other way round; a symlinked host file keeps its link.
	real := filepath.Join(dir, "dotfiles", "creds")
	write(real, "v3", t0.Add(2*time.Minute))
	os.Remove(host)
	os.Symlink(real, host)
	if err := syncFile(ctx, host, sb, lock); err != nil {
		t.Fatal(err)
	}
	if read(sb) != "v3" {
		t.Fatalf("sandbox = %q", read(sb))
	}
	write(sb, "v4", t0.Add(3*time.Minute))
	if err := syncFile(ctx, host, sb, lock); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(host); fi.Mode()&os.ModeSymlink == 0 || read(real) != "v4" {
		t.Fatalf("symlink replaced or target not updated: %q", read(real))
	}
}
