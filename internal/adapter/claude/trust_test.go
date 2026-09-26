package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fleet/internal/adapter"
)

// config is a trimmed ~/.claude.json with an untrusted and a trusted project.
const config = `{
  "numStartups": 12,
  "oauthAccount": {"emailAddress": "a@b.c"},
  "projects": {
    "/home/u/old": {"allowedTools": ["Bash"], "hasTrustDialogAccepted": false, "lastCost": 0.2299228},
    "/home/u/ok": {"hasTrustDialogAccepted": true}
  },
  "theme": "dark"
}`

func project(t *testing.T, data []byte, dir string) map[string]any {
	t.Helper()
	var c struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c.Projects[dir]
}

func TestSetTrusted(t *testing.T) {
	out, changed, err := setTrusted([]byte(config), "/home/u/new")
	if err != nil || !changed {
		t.Fatalf("new project: changed=%v err=%v", changed, err)
	}
	p := project(t, out, "/home/u/new")
	if p["hasTrustDialogAccepted"] != true || len(p) != len(projectDefaults) {
		t.Errorf("new project entry = %v", p)
	}
	for _, keep := range []string{`"numStartups": 12`, `"emailAddress": "a@b.c"`, `"lastCost": 0.2299228`, `"theme": "dark"`} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("output lost %s:\n%s", keep, out)
		}
	}

	out, changed, err = setTrusted([]byte(config), "/home/u/old")
	if err != nil || !changed {
		t.Fatalf("existing project: changed=%v err=%v", changed, err)
	}
	p = project(t, out, "/home/u/old")
	if p["hasTrustDialogAccepted"] != true || p["lastCost"] != 0.2299228 || len(p["allowedTools"].([]any)) != 1 {
		t.Errorf("existing project entry = %v", p)
	}

	if out, changed, err = setTrusted([]byte(config), "/home/u/ok"); err != nil || changed || string(out) != config {
		t.Errorf("already trusted: changed=%v err=%v", changed, err)
	}
	if _, _, err := setTrusted([]byte(`{}`), "/x"); err != nil {
		t.Errorf("no projects key: %v", err)
	}
	for _, bad := range []string{``, `[]`, `null`, `{"projects": []}`} {
		if _, _, err := setTrusted([]byte(bad), "/x"); err == nil {
			t.Errorf("setTrusted(%q): no error", bad)
		}
	}
}

func TestTrustFolder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, ".claude.json")

	// No config yet: nothing is created.
	if err := trustFolder(ctx, file, "/w"); err != nil {
		t.Fatal(err)
	}
	if fileExists(file) {
		t.Fatal("trustFolder created a config file")
	}

	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stale lock left by a crashed Claude is taken over.
	lock := file + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lock, old, old)
	if err := trustFolder(ctx, file, "/w"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if project(t, data, "/w")["hasTrustDialogAccepted"] != true {
		t.Fatalf("not trusted:\n%s", data)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if fileExists(lock) {
		t.Error("lock left behind")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("stray files: %v", entries)
	}

	// A live lock is waited for.
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Remove(lock)
	}()
	if err := trustFolder(ctx, file, "/w2"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(file)
	if project(t, data, "/w2")["hasTrustDialogAccepted"] != true || project(t, data, "/w")["hasTrustDialogAccepted"] != true {
		t.Fatalf("second trust:\n%s", data)
	}

	// ... but not forever.
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := trustFolder(cctx, file, "/w3"); err == nil {
		t.Fatal("trustFolder ignored a held lock")
	}
}

func TestTrustFolderSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles.json")
	link := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(real, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := trustFolder(context.Background(), link, "/w"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	data, _ := os.ReadFile(real)
	if project(t, data, "/w")["hasTrustDialogAccepted"] != true {
		t.Fatalf("target not updated:\n%s", data)
	}
}

func TestGlobalConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, _ := globalConfigFile(); got != filepath.Join(home, ".claude.json") {
		t.Errorf("default = %s", got)
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if got, _ := globalConfigFile(); got != filepath.Join(cfg, ".claude.json") {
		t.Errorf("CLAUDE_CONFIG_DIR = %s", got)
	}
	os.WriteFile(filepath.Join(cfg, ".config.json"), []byte("{}"), 0o600)
	if got, _ := globalConfigFile(); got != filepath.Join(cfg, ".config.json") {
		t.Errorf("legacy = %s", got)
	}
}

func TestLaunchTrustDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	file := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(fakeClaude(t), nil)
	req := adapter.LaunchRequest{AgentID: "a1", Cwd: "/w/sub", FleetBinary: "/bin/fleet", StateDir: t.TempDir()}
	if _, err := a.Launch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) != config {
		t.Fatal("config changed without TrustDir")
	}
	req.TrustDir = "/w"
	if _, err := a.Launch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if project(t, data, "/w")["hasTrustDialogAccepted"] != true {
		t.Fatalf("not trusted:\n%s", data)
	}
}

// trustScreen is Claude Code 2.1.280's trust dialog as tmux captures it.
const trustScreen = `────────────────────────────────────────────────────────────
 Accessing workspace:

 /tmp/trusttest/code/repo/sub

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team).
 If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestDetectPrompt(t *testing.T) {
	d := New("", nil).(adapter.PromptDetector)
	if detail, ok := d.DetectPrompt(trustScreen); !ok || !strings.Contains(detail, "trust") {
		t.Errorf("trust dialog: %q, %v", detail, ok)
	}
	if _, ok := d.DetectPrompt("╭───╮\n│ > \n╰───╯\n  ? for shortcuts\n"); ok {
		t.Error("prompt screen detected as a dialog")
	}
}
