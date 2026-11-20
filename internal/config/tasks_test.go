package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("FLEET_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTasksConfig(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tok, []byte(" rome_sat_x \n"), 0o600)
	writeConfig(t, `
[[tasks]]
kind = "rome"
url = "https://rome.example.com"
token_file = "`+tok+`"
model = "opus"
effort = "high"
instructions = "Run the tests."

[[tasks.project]]
key = "ROME"
path = "/srv/rome"

[[tasks.project]]
key = "web"
root = "code"
path = "web"
adapter = "codex"
model = "gpt-6"
instructions = "Use pnpm."
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	src := c.Tasks[0]
	if src.SourceName() != "rome" || src.MaxAgentsOrDefault() != DefaultTaskMaxAgents || src.CloseAfterOrDefault() != DefaultTaskCloseAfter {
		t.Errorf("source = %+v", src)
	}
	for v, want := range map[string]time.Duration{"": time.Hour, "30m": 30 * time.Minute, " 1h30m ": 90 * time.Minute, "off": 0, "0": 0} {
		if got := (TaskSource{CloseAfter: v}).CloseAfterOrDefault(); got != want {
			t.Errorf("close_after %q = %v, want %v", v, got, want)
		}
	}
	if s, err := src.Secret(); err != nil || s != "rome_sat_x" {
		t.Errorf("Secret() = %q, %v", s, err)
	}
	rome, ok := src.Project("rome")
	if !ok {
		t.Fatal("project ROME not found case-insensitively")
	}
	if a := src.Agent(rome); a.Adapter != "claude" || a.Model != "opus" || a.Effort != "high" || a.Instructions != "Run the tests." {
		t.Errorf("ROME agent = %+v", a)
	}
	web, _ := src.Project("WEB")
	// A project's model drops the source's effort, which may not fit it.
	if a := src.Agent(web); a.Adapter != "codex" || a.Model != "gpt-6" || a.Effort != "" || a.Instructions != "Run the tests.\n\nUse pnpm." {
		t.Errorf("WEB agent = %+v", a)
	}
	if r, rel, abs := web.ProjectDir(); r != "code" || rel != "web" || abs != "" {
		t.Errorf("WEB dir = %q %q %q", r, rel, abs)
	}

	t.Setenv("FLEET_TEST_TOKEN", "")
	if _, err := (TaskSource{TokenEnv: "FLEET_TEST_TOKEN"}).Secret(); err == nil {
		t.Error("an unset token_env gave a token")
	}
}

func TestTasksConfigErrors(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{`[[tasks]]
kind = "jira"
url = "https://x"`, "kind must be one of rome"},
		{`[[tasks]]
kind = "rome"
url = "rome.example.com"`, "url must start with"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
[[tasks]]
kind = "rome"
url = "https://b"`, "a second source named"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
[[tasks.project]]
key = "ROME"
path = "rel/path"`, "path must be absolute"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
[[tasks.project]]
key = "ROME"
path = "/a"
[[tasks.project]]
key = "rome"
path = "/b"`, "listed twice"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
isolation = "clone"`, "isolation must be"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
close_after = "never"`, "close_after is a duration"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
close_after = "-1h"`, "close_after is a duration"},
		{`[[tasks]]
kind = "rome"
url = "https://a"
[[tasks.project]]
key = "ROME-1"
path = "/a"`, "is not a project key"},
	} {
		writeConfig(t, tt.body)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Load(%q) = %v, want an error with %q", tt.body, err, tt.want)
		}
	}
}

func TestEditTasks(t *testing.T) {
	writeConfig(t, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.MapTaskProject("", "ROME", TaskProjectPatch{}); !errors.Is(err, ErrUnknownTaskSource) {
		t.Errorf("map without a source: %v", err)
	}
	if err := c.AddTaskSource(TaskSource{URL: "https://rome.example.com", TokenFile: "~/tok"}); err != nil {
		t.Fatal(err)
	}
	if c.Tasks[0].Kind != "rome" || c.Tasks[0].SourceName() != "rome" {
		t.Errorf("added = %+v", c.Tasks[0])
	}
	if err := c.AddTaskSource(TaskSource{URL: "https://other.example.com"}); err == nil {
		t.Error("a second source named rome was added")
	}
	if err := c.AddTaskSource(TaskSource{Name: "bad", URL: "ftp://x"}); err == nil || len(c.Tasks) != 1 {
		t.Errorf("an invalid source: %v, %d sources", err, len(c.Tasks))
	}

	str := func(s string) *string { return &s }
	if _, _, err := c.MapTaskProject("", "rome", TaskProjectPatch{Model: str("opus")}); err == nil {
		t.Error("a new mapping without a folder was accepted")
	}
	p, created, err := c.MapTaskProject("", "rome", TaskProjectPatch{Path: str("/srv/rome"), Instructions: str("Use pnpm.")})
	if err != nil || !created || p.Key != "ROME" || p.Path != "/srv/rome" {
		t.Fatalf("map = %+v, %v, %v", p, created, err)
	}
	// An update changes only what it names.
	args := []string{"--permission-mode", "acceptEdits"}
	p, created, err = c.MapTaskProject("rome", "ROME", TaskProjectPatch{Model: str("opus"), Args: &args})
	if err != nil || created || p.Path != "/srv/rome" || p.Instructions != "Use pnpm." || p.Model != "opus" || len(p.Args) != 2 {
		t.Errorf("update = %+v, %v, %v", p, created, err)
	}
	if _, _, err := c.MapTaskProject("rome", "ROME", TaskProjectPatch{Path: str("relative")}); err == nil || c.Tasks[0].Projects[0].Path != "/srv/rome" {
		t.Errorf("a relative path: %v; the mapping must stay %q", err, c.Tasks[0].Projects[0].Path)
	}
	p, _, err = c.MapTaskProject("rome", "ROME", TaskProjectPatch{Root: str("code"), Path: str("rome")})
	if err != nil || p.Root != "code" || p.Path != "rome" {
		t.Errorf("root mapping = %+v, %v", p, err)
	}

	for _, tt := range []struct {
		key    string
		values []string
		check  func(TaskSource) bool
	}{
		{"max_agents", []string{"5"}, func(s TaskSource) bool { return s.MaxAgents == 5 }},
		{"close_after", []string{"30m"}, func(s TaskSource) bool { return s.CloseAfterOrDefault() == 30*time.Minute }},
		{"close_after", []string{"off"}, func(s TaskSource) bool { return s.CloseAfter == "off" && s.CloseAfterOrDefault() == 0 }},
		{"close_after", nil, func(s TaskSource) bool { return s.CloseAfter == "" && s.CloseAfterOrDefault() == time.Hour }},
		{"disabled", []string{"true"}, func(s TaskSource) bool { return s.Disabled }},
		{"disabled", nil, func(s TaskSource) bool { return !s.Disabled }},
		{"model", []string{"sonnet"}, func(s TaskSource) bool { return s.Model == "sonnet" }},
		{"args", []string{"--permission-mode", "acceptEdits"}, func(s TaskSource) bool { return len(s.Args) == 2 }},
		{"args", nil, func(s TaskSource) bool { return s.Args == nil }},
		{"token_env", []string{"ROME_TOKEN"}, func(s TaskSource) bool { return s.TokenEnv == "ROME_TOKEN" && s.TokenFile == "" }},
		{"webhook", []string{"whk_1"}, func(s TaskSource) bool { return s.Webhook == "whk_1" }},
	} {
		if err := c.SetTaskOption("", tt.key, tt.values); err != nil || !tt.check(c.Tasks[0]) {
			t.Errorf("set %s %v: %v, %+v", tt.key, tt.values, err, c.Tasks[0])
		}
	}
	for _, bad := range [][]string{{"isolation", "clone"}, {"max_agents", "x"}, {"close_after", "soon"}, {"color", "red"}, {"url", "a", "b"}} {
		if err := c.SetTaskOption("", bad[0], bad[1:]); err == nil {
			t.Errorf("set %v was accepted", bad)
		}
	}

	// It survives a save and a load.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s := c2.Tasks[0]; s.MaxAgents != 5 || s.Model != "sonnet" || s.TokenEnv != "ROME_TOKEN" || len(s.Projects) != 1 ||
		s.Projects[0].Root != "code" || s.Projects[0].Model != "opus" || len(s.Projects[0].Args) != 2 || s.Projects[0].Instructions != "Use pnpm." {
		t.Errorf("loaded = %+v", s)
	}

	if err := c.UnmapTaskProject("", "rome"); err != nil || len(c.Tasks[0].Projects) != 0 {
		t.Errorf("unmap: %v, %+v", err, c.Tasks[0].Projects)
	}
	if err := c.UnmapTaskProject("", "rome"); err == nil {
		t.Error("unmapping an unmapped project succeeded")
	}
	if err := c.RemoveTaskSource("rome"); err != nil || len(c.Tasks) != 0 {
		t.Errorf("remove: %v, %d sources", err, len(c.Tasks))
	}
}
