package detect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin writes an executable script named name into dir that prints out
// for --version and appends a line to dir/calls on every run.
func fakeBin(t *testing.T, dir, name, out string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\necho x >> '" + filepath.Join(dir, "calls") + "'\necho '" + out + "'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func calls(t *testing.T, dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return strings.Count(string(b), "x")
}

func TestDetectWellKnownDirAndCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // empty PATH dir
	want := fakeBin(t, dir, "fakeagent", "2.1.280 (Claude Code)")

	b := &Binary{Name: "fakeagent", Dirs: []string{"/nonexistent", dir}}
	for i := 0; i < 3; i++ {
		d := b.Detect(context.Background())
		if !d.Available || d.Path != want || d.Version != "2.1.280" {
			t.Fatalf("Detect = %+v", d)
		}
	}
	if n := calls(t, dir); n != 1 {
		t.Errorf("--version ran %d times, want 1 (cached)", n)
	}
}

func TestDetectPathBeforeDirs(t *testing.T) {
	pathDir, other := t.TempDir(), t.TempDir()
	want := fakeBin(t, pathDir, "fakeagent", "codex-cli 0.154.0")
	fakeBin(t, other, "fakeagent", "9.9.9")
	t.Setenv("PATH", pathDir)

	d := (&Binary{Name: "fakeagent", Dirs: []string{other}}).Detect(context.Background())
	if d.Path != want || d.Version != "0.154.0" {
		t.Fatalf("Detect = %+v", d)
	}
}

func TestDetectOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	p := fakeBin(t, dir, "custom", "no version here")

	tests := []struct {
		override string
		ok       bool
	}{
		{p, true},
		{"custom", true}, // looked up in PATH
		{filepath.Join(dir, "missing"), false},
		{dir, false}, // a directory
	}
	for _, tt := range tests {
		d := (&Binary{Name: "fakeagent", Override: tt.override}).Detect(context.Background())
		if d.Available != tt.ok {
			t.Errorf("override %q: %+v", tt.override, d)
		}
		if tt.ok && (d.Path != p || d.Version != "no version here") {
			t.Errorf("override %q: %+v", tt.override, d)
		}
		if !tt.ok && d.Reason == "" {
			t.Errorf("override %q: no reason", tt.override)
		}
	}
}

func TestDetectNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := (&Binary{Name: "fakeagent", Dirs: []string{t.TempDir()}}).Detect(context.Background())
	if d.Available || !strings.Contains(d.Reason, "fakeagent not found") {
		t.Fatalf("Detect = %+v", d)
	}
}

func TestDetectVersionFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := (&Binary{Name: "broken", Override: p}).Detect(context.Background())
	if d.Available || d.Path != p || d.Reason == "" {
		t.Fatalf("Detect = %+v", d)
	}
}
