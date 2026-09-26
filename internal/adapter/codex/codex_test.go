package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

func fakeCodex(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'codex-cli 0.154.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetect(t *testing.T) {
	bin := fakeCodex(t)
	d := New(bin, nil).Detect(context.Background())
	if !d.Available || d.Path != bin || d.Version != "0.154.0" {
		t.Fatalf("Detect = %+v", d)
	}
}

// Golden hashes observed from Codex 0.154.0: the first was written to
// config.toml by the TUI's "trust" action (no timeout given, so Codex
// normalized it to 600); the second is a self-computed hash Codex accepted.
func TestTrustHash(t *testing.T) {
	tests := []struct {
		key, cmd string
		timeout  int
		want     string
	}{
		{"session_start", "/tmp/fleet-adapter-probe/hook.sh codex SessionStart", 600,
			"sha256:8f1c1db1976d1c5843710304486c886686e7466f194dd8dce02bb346044f3d4f"},
		{"session_start", "/tmp/fleet-adapter-probe/hook.sh codex SessionStart", 10,
			"sha256:11f26685b1811b78ba340035243e557f440b7510882981e7890fb3a2e7a983a3"},
	}
	for _, tt := range tests {
		if got := trustHash(tt.key, tt.cmd, tt.timeout); got != tt.want {
			t.Errorf("trustHash(%q, %q, %d) = %s, want %s", tt.key, tt.cmd, tt.timeout, got, tt.want)
		}
	}
}

func TestLaunch(t *testing.T) {
	bin := fakeCodex(t)
	a := New(bin, []string{"-m", "gpt-6"})
	spec, err := a.Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "ag1", Cwd: "/work", Prompt: "-x fails", ExtraArgs: []string{"--search"},
		FleetBinary: "/opt/my fleet/fleet", StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", spec.SessionID)
	}
	argv := spec.Argv
	if argv[0] != bin || argv[1] != "-c" || argv[2] != "check_for_update_on_startup=false" {
		t.Fatalf("head %q", argv[:3])
	}
	if tail := argv[len(argv)-5:]; !slices.Equal(tail, []string{"-m", "gpt-6", "--search", "--", "-x fails"}) {
		t.Errorf("tail %q", tail)
	}

	// Every -c value must be valid TOML as Codex parses it (key=value).
	var overrides []string
	for i := 1; i < len(argv)-5; i += 2 {
		if argv[i] != "-c" {
			t.Fatalf("argv[%d] = %q, want -c", i, argv[i])
		}
		overrides = append(overrides, argv[i+1])
	}
	doc := strings.Join(overrides, "\n")
	var cfg struct {
		Hooks map[string]any `toml:"hooks"`
	}
	if _, err := toml.Decode(doc, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, doc)
	}
	state, _ := cfg.Hooks["state"].(map[string]any)
	for _, ev := range hookEvents {
		// hooks.<Event> = [ {hooks = [ {handler} ]} ]
		groups, _ := cfg.Hooks[ev.Name].([]any)
		if len(groups) != 1 {
			t.Fatalf("%s: %#v", ev.Name, cfg.Hooks[ev.Name])
		}
		group, _ := groups[0].(map[string]any)
		handlers, _ := group["hooks"].([]any)
		if len(handlers) != 1 {
			t.Fatalf("%s: %#v", ev.Name, group)
		}
		h, _ := handlers[0].(map[string]any)
		wantCmd := "'/opt/my fleet/fleet' hook --agent ag1 --adapter codex " + ev.Name
		if h["type"] != "command" || h["command"] != wantCmd || h["timeout"] != int64(hookTimeout) {
			t.Errorf("%s: %#v", ev.Name, h)
		}
		key := sessionFlagsSource + ":" + ev.Key + ":0:0"
		entry, _ := state[key].(map[string]any)
		if entry["trusted_hash"] != TrustHash(ev.Key, wantCmd) {
			t.Errorf("%s: trust entry %#v", key, entry)
		}
	}
}

func TestLaunchNoPrompt(t *testing.T) {
	spec, err := New(fakeCodex(t), nil).Launch(context.Background(), adapter.LaunchRequest{AgentID: "a", FleetBinary: "/f"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Argv, "--") {
		t.Errorf("unexpected -- in %q", spec.Argv)
	}
	if _, err := New(filepath.Join(t.TempDir(), "nope"), nil).Launch(context.Background(), adapter.LaunchRequest{FleetBinary: "/f"}); err == nil {
		t.Error("missing binary: no error")
	}
}

func TestHandleHook(t *testing.T) {
	const sid = "01a0cfb6-4234-72f2-8548-097d2eff3d39"
	// Hook payloads as captured from Codex 0.154.0 (paths shortened).
	tests := []struct {
		event, payload string
		ok             bool
		state          fleetv1.AgentState
		detail, sid    string
	}{
		{"SessionStart", `{"session_id":"` + sid + `","transcript_path":"/r.jsonl","cwd":"/w","hook_event_name":"SessionStart","model":"gpt-6-astra","permission_mode":"bypassPermissions","source":"startup"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"SessionStart", `{"session_id":"` + sid + `","source":"clear"}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"UserPromptSubmit", `{"session_id":"` + sid + `","turn_id":"01a0cfb6-accc","cwd":"/w","hook_event_name":"UserPromptSubmit","model":"gpt-6-astra","permission_mode":"default","prompt":"hi"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"PermissionRequest", `{"session_id":"` + sid + `","turn_id":"t","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"cargo publish"}}`,
			true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Bash: cargo publish", sid},
		{"PostToolUse", `{"session_id":"` + sid + `","tool_name":"Bash"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"Stop", `{"session_id":"` + sid + `","turn_id":"t","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"Done."}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"notify", `{"type":"agent-turn-complete","thread-id":"` + sid + `","turn-id":"t","cwd":"/w","input-messages":["hi"],"last-assistant-message":"Done."}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"notify", `{"type":"approval-requested","thread-id":"` + sid + `"}`,
			true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "", sid},
		{"notify", `{"type":"something-new"}`, false, 0, "", ""},
		{"SubagentStart", `{}`, false, 0, "", ""},
	}
	a := New("", nil)
	for _, tt := range tests {
		u, ok := a.HandleHook(adapter.HookEvent{AgentID: "a", Event: tt.event, Payload: []byte(tt.payload)})
		if ok != tt.ok {
			t.Errorf("%s %s: ok = %v", tt.event, tt.payload, ok)
			continue
		}
		if ok && (u.State != tt.state || u.Detail != tt.detail || u.SessionID != tt.sid) {
			t.Errorf("%s %s: %+v, want state %v detail %q sid %q", tt.event, tt.payload, u, tt.state, tt.detail, tt.sid)
		}
	}
}

func TestLaunchTrustDir(t *testing.T) {
	bin := fakeCodex(t)
	dir := `/work/odd "dir" \ name`
	for _, trust := range []string{"", dir} {
		spec, err := New(bin, nil).Launch(context.Background(), adapter.LaunchRequest{AgentID: "a", FleetBinary: "/f", TrustDir: trust})
		if err != nil {
			t.Fatal(err)
		}
		var projects []string
		for i, arg := range spec.Argv {
			if i > 0 && spec.Argv[i-1] == "-c" && strings.HasPrefix(arg, "projects=") {
				projects = append(projects, arg)
			}
		}
		if trust == "" {
			if len(projects) != 0 {
				t.Errorf("no TrustDir: %q", projects)
			}
			continue
		}
		if len(projects) != 1 {
			t.Fatalf("TrustDir: %q", spec.Argv)
		}
		var cfg struct {
			Projects map[string]struct {
				TrustLevel string `toml:"trust_level"`
			} `toml:"projects"`
		}
		if _, err := toml.Decode(projects[0], &cfg); err != nil {
			t.Fatalf("%v: %s", err, projects[0])
		}
		if len(cfg.Projects) != 1 || cfg.Projects[dir].TrustLevel != "trusted" {
			t.Errorf("%s decodes to %+v", projects[0], cfg.Projects)
		}
	}
}

func TestDetectPrompt(t *testing.T) {
	d := New("", nil).(adapter.PromptDetector)
	// Codex 0.154.0's startup dialogs as tmux captures them.
	for _, screen := range []string{
		`> You are in /tmp/trusttest/cx/repo/sub

  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit

  Press enter to continue`,
		`  Hooks need review
  2 hooks are new or changed.
  Hooks can run outside the sandbox after you trust them.

› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)`,
	} {
		if detail, ok := d.DetectPrompt(screen); !ok || detail == "" {
			t.Errorf("not detected:\n%s", screen)
		}
	}
	if _, ok := d.DetectPrompt("› Ask Codex to do anything\n\n  100% context left"); ok {
		t.Error("composer detected as a dialog")
	}
}
