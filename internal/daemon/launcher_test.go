package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLauncherShortArgvUnchanged(t *testing.T) {
	argv := []string{"claude", "--", "hi"}
	got, err := launcher(t.TempDir(), argv)
	if err != nil || !slices.Equal(got, argv) {
		t.Fatalf("launcher = %q, %v; want argv unchanged", got, err)
	}
}

func TestLauncherLongArgvRunsExactly(t *testing.T) {
	dir := t.TempDir()
	prompt := strings.Repeat("it's a \"long\" $HOME `x` \\ line;\n", 2000)
	out := filepath.Join(dir, "out")
	argv := []string{"/bin/sh", "-c", `printf %s "$1" > "$2"`, "sh", prompt, out}
	got, err := launcher(dir, argv)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "/bin/sh" {
		t.Fatalf("launcher = %q, want a script", got)
	}
	if b, err := exec.Command(got[0], got[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("script: %v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != prompt {
		t.Fatalf("prompt arrived changed (%d bytes, want %d): %v", len(b), len(prompt), err)
	}
	if _, err := os.Stat(got[1]); !os.IsNotExist(err) {
		t.Fatalf("script still exists: %v", err)
	}
}
