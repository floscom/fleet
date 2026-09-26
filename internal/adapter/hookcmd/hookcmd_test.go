package hookcmd

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCommand(t *testing.T) {
	got := Command("/opt/fleet/bin/fleet", "a1b2", "claude", "Stop")
	want := "/opt/fleet/bin/fleet hook --agent a1b2 --adapter claude Stop"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"", "plain", "with space", "it's", `a"b\c`, "$HOME;rm -rf /", "é ü"} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != s {
			t.Errorf("Quote(%q) round trip = %q", s, out)
		}
	}
}

func TestToolDetail(t *testing.T) {
	tests := []struct {
		tool, input, want string
	}{
		{"Bash", `{"command":"rm -rf build","description":"clean"}`, "Bash: rm -rf build"},
		{"Write", `{"file_path":"/x/y.go","content":"..."}`, "Write: /x/y.go"},
		{"mcp__x__y", `{"q":1}`, "mcp__x__y"},
		{"", `{"command":"ls"}`, "ls"},
		{"Bash", `not json`, "Bash"},
	}
	for _, tt := range tests {
		if got := ToolDetail(tt.tool, []byte(tt.input)); got != tt.want {
			t.Errorf("ToolDetail(%q, %s) = %q, want %q", tt.tool, tt.input, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("  first\nsecond"); got != "first" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 300)
	got := Truncate(long)
	if len(got) > maxDetail || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "éé") {
		t.Errorf("len %d: %q", len(got), got)
	}
}
