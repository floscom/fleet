package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

func fakeClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '2.1.280 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetect(t *testing.T) {
	bin := fakeClaude(t)
	d := New(bin, nil).Detect(context.Background())
	if !d.Available || d.Path != bin || d.Version != "2.1.280" {
		t.Fatalf("Detect = %+v", d)
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestLaunch(t *testing.T) {
	bin := fakeClaude(t)
	state := filepath.Join(t.TempDir(), "agents", "a1")
	a := New(bin, []string{"--model", "opus"})

	tests := []struct {
		name   string
		prompt string
		extra  []string
		tail   []string
	}{
		{"no prompt", "", nil, []string{"--model", "opus"}},
		{"prompt and extra", "fix the tests", []string{"--effort", "high"},
			[]string{"--model", "opus", "--effort", "high", "--", "fix the tests"}},
		{"dash prompt", "-v is broken", nil, []string{"--model", "opus", "--", "-v is broken"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := a.Launch(context.Background(), adapter.LaunchRequest{
				AgentID: "a1", Cwd: "/work", Prompt: tt.prompt, ExtraArgs: tt.extra,
				FleetBinary: "/usr/bin/fleet", StateDir: state,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !uuidRE.MatchString(spec.SessionID) {
				t.Errorf("SessionID %q is not a v4 uuid", spec.SessionID)
			}
			settings := filepath.Join(state, SettingsFile)
			want := append([]string{bin, "--session-id", spec.SessionID, "--settings", settings}, tt.tail...)
			if !slices.Equal(spec.Argv, want) {
				t.Errorf("Argv =\n %q\nwant\n %q", spec.Argv, want)
			}
			if !slices.Contains(spec.UnsetEnv, "ANTHROPIC_API_KEY") {
				t.Errorf("UnsetEnv = %v", spec.UnsetEnv)
			}
		})
	}
}

func TestLaunchResumeOmitsSessionID(t *testing.T) {
	bin := fakeClaude(t)
	state := t.TempDir()
	settings := filepath.Join(state, SettingsFile)
	tests := []struct {
		extra  []string
		resume bool
	}{
		{nil, false},
		{[]string{"--continue"}, true},
		{[]string{"-c"}, true},
		{[]string{"--resume", "abc"}, true},
		{[]string{"-r"}, true},
		{[]string{"--resume=abc"}, true},
		{[]string{"--model", "opus"}, false},
		{[]string{"--", "-c"}, false},
	}
	for _, tt := range tests {
		spec, err := New(bin, nil).Launch(context.Background(), adapter.LaunchRequest{
			AgentID: "a", ExtraArgs: tt.extra, FleetBinary: "/f", StateDir: state,
		})
		if err != nil {
			t.Fatal(err)
		}
		has := slices.Contains(spec.Argv, "--session-id")
		if has == tt.resume || (spec.SessionID == "") != tt.resume {
			t.Errorf("%q: argv %q, session id %q", tt.extra, spec.Argv, spec.SessionID)
		}
		if tt.resume {
			want := append([]string{bin, "--settings", settings}, tt.extra...)
			if !slices.Equal(spec.Argv, want) {
				t.Errorf("Argv = %q, want %q", spec.Argv, want)
			}
		}
	}
}

func TestLaunchUniqueSessionIDs(t *testing.T) {
	a := New(fakeClaude(t), nil)
	req := adapter.LaunchRequest{AgentID: "a", FleetBinary: "/f", StateDir: t.TempDir()}
	s1, _ := a.Launch(context.Background(), req)
	s2, _ := a.Launch(context.Background(), req)
	if s1.SessionID == s2.SessionID {
		t.Fatal("session ids repeat")
	}
}

func TestLaunchErrors(t *testing.T) {
	ok := adapter.LaunchRequest{AgentID: "a", FleetBinary: "/f", StateDir: t.TempDir()}
	if _, err := New(filepath.Join(t.TempDir(), "missing"), nil).Launch(context.Background(), ok); err == nil {
		t.Error("missing binary: no error")
	}
	noState := ok
	noState.StateDir = ""
	if _, err := New(fakeClaude(t), nil).Launch(context.Background(), noState); err == nil {
		t.Error("missing StateDir: no error")
	}
}

func TestSettingsFile(t *testing.T) {
	state := t.TempDir()
	_, err := New(fakeClaude(t), nil).Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "ag 1", FleetBinary: "/opt/my fleet/fleet", StateDir: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, SettingsFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)

	// Parse generically to check the exact JSON shape Claude Code expects.
	var raw struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if len(raw.Hooks) != len(HookEvents) {
		t.Errorf("events = %d, want %d", len(raw.Hooks), len(HookEvents))
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Notification", "Stop"} {
		groups := raw.Hooks[ev]
		if len(groups) != 1 || groups[0].Matcher == nil || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, groups)
		}
		h := groups[0].Hooks[0]
		want := "'/opt/my fleet/fleet' hook --agent 'ag 1' --adapter claude " + ev
		if h.Type != "command" || h.Command != want || h.Timeout != hookTimeout {
			t.Errorf("%s: %+v, want command %q", ev, h, want)
		}
	}
}

func TestHandleHook(t *testing.T) {
	const sid = "3f1c2a4e-7b5d-4e8a-9c1f-2d3e4f5a6b7c"
	// Payloads as captured from Claude Code 2.1.280 (paths shortened).
	tests := []struct {
		event, payload string
		ok             bool
		state          fleetv1.AgentState
		detail, sid    string
	}{
		{"SessionStart", `{"session_id":"` + sid + `","transcript_path":"/t.jsonl","cwd":"/w","hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5-5"}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"UserPromptSubmit", `{"session_id":"` + sid + `","cwd":"/w","prompt_id":"88d6","permission_mode":"auto","hook_event_name":"UserPromptSubmit","prompt":"hi"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"Notification", `{"session_id":"` + sid + `","hook_event_name":"Notification","message":"Claude is waiting for your input","notification_type":"idle_prompt"}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"Notification", `{"session_id":"` + sid + `","hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`,
			false, 0, "", ""},
		{"Notification", `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`,
			true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Claude needs your permission to use Bash", ""},
		{"Notification", `{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", ""},
		{"Notification", `{"message":"Signed in","notification_type":"auth_success"}`, false, 0, "", ""},
		{"Notification", `{"message":"done","notification_type":"elicitation_complete"}`, false, 0, "", ""},
		{"PermissionRequest", `{"session_id":"` + sid + `","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"npm test","description":"Run tests"}}`,
			true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Bash: npm test", sid},
		{"PermissionRequest", `{"session_id":"` + sid + `","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which color do you prefer?","header":"Color","multiSelect":false,"options":[{"label":"Red"},{"label":"Blue"}]}]}}`,
			true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Claude asks: Which color do you prefer?", sid},
		{"PostToolUse", `{"session_id":"` + sid + `","tool_name":"Bash","tool_input":{},"tool_response":{}}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"PostToolUseFailure", `{"session_id":"` + sid + `","hook_event_name":"PostToolUseFailure","tool_name":"Bash","error":"Interrupted","is_interrupt":true}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"PostToolUseFailure", `{"session_id":"` + sid + `","hook_event_name":"PostToolUseFailure","tool_name":"Bash","error":"exit 1"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"PermissionDenied", `{"session_id":"` + sid + `","hook_event_name":"PermissionDenied","tool_name":"Bash"}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"Stop", `{"session_id":"` + sid + `","hook_event_name":"Stop","stop_hook_active":false}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "", sid},
		{"StopFailure", `{"session_id":"` + sid + `","error":"rate_limit"}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "rate_limit", sid},
		{"StopFailure", `{"error":{"type":"overloaded"}}`,
			true, fleetv1.AgentState_AGENT_STATE_IDLE, "turn ended with an API error", ""},
		{"Stop", ``, true, fleetv1.AgentState_AGENT_STATE_IDLE, "", ""},
		{"SubagentStop", `{"session_id":"` + sid + `","hook_event_name":"SubagentStop","agent_id":"a98756a7d8eaf74a5","agent_type":"general-purpose","stop_hook_active":false}`,
			true, fleetv1.AgentState_AGENT_STATE_WORKING, "", sid},
		{"SubagentStop", `{"session_id":"` + sid + `","hook_event_name":"SubagentStop"}`, false, 0, "", ""},
		{"SessionEnd", `{}`, false, 0, "", ""},
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

func TestHandleHookSubagent(t *testing.T) {
	a := New("", nil)
	// Captured from a background general-purpose subagent (2.1.280).
	for _, ev := range []string{"PreToolUse", "PostToolUse", "PermissionRequest", "SubagentStop"} {
		u, ok := a.HandleHook(adapter.HookEvent{Event: ev, Payload: []byte(`{"session_id":"s","hook_event_name":"` + ev + `","agent_id":"a98756a7d8eaf74a5","agent_type":"general-purpose","tool_name":"Bash","tool_input":{"command":"ls /"}}`)})
		if !ok || u.Subagent != "a98756a7d8eaf74a5" {
			t.Errorf("%s: ok %v, Subagent %q", ev, ok, u.Subagent)
		}
	}
	if u, _ := a.HandleHook(adapter.HookEvent{Event: "PostToolUse", Payload: []byte(`{"session_id":"s","tool_name":"Bash"}`)}); u.Subagent != "" {
		t.Errorf("the agent itself: Subagent %q", u.Subagent)
	}
}

func TestHandleHookCall(t *testing.T) {
	a := New("", nil)
	call := func(event, payload string) string {
		t.Helper()
		u, ok := a.HandleHook(adapter.HookEvent{Event: event, Payload: []byte(payload)})
		if !ok {
			t.Fatalf("%s ignored", event)
		}
		return u.Call
	}
	// Every event of a call names it alike, although PermissionRequest has
	// no tool_use_id and PostToolUse adds the answers to AskUserQuestion.
	const q = `"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which color?","options":[{"label":"Red"}]}]`
	ask := call("PermissionRequest", `{`+q+`}}`)
	answered := call("PostToolUse", `{"tool_use_id":"toolu_1",`+q+`,"answers":{"Which color?":"Red"}}}`)
	if ask != "Claude asks: Which color?" || answered != ask {
		t.Errorf("AskUserQuestion: %q, then %q", ask, answered)
	}
	const bash = `"tool_name":"Bash","tool_input":{"command":"touch x","description":"Create x"}`
	for _, ev := range []string{"PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure", "PermissionDenied"} {
		if got := call(ev, `{`+bash+`}`); got != "Bash: touch x" {
			t.Errorf("%s: Call %q", ev, got)
		}
	}
	// An interrupt ends the turn, not one call.
	if got := call("PostToolUseFailure", `{`+bash+`,"is_interrupt":true}`); got != "" {
		t.Errorf("interrupt: Call %q", got)
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop", "SessionStart"} {
		if got := call(ev, `{}`); got != "" {
			t.Errorf("%s: Call %q", ev, got)
		}
	}
}

// Screens of Claude Code 2.1.280, as tmux captures them (rows trimmed).
const (
	questionScreen = `● 2 background agents launched (↓ to manage)
   ├ Agent (Research InternetX AutoDNS JSON API)
   └ Explore (Extract agent-first CLI conventions)
────────────────────────────────────────────────
←  ☒ Name  ☐ Scope  ☐ Creds  ✔ Submit  →

Which API areas should v1 cover?

❯ 1. [ ] Domains
         list/search/info, availability check, create/renew/transfer/cancel
  2. [ ] DNS zones
         list/info, add/remove/replace records
  5. [ ] Type something
     Next
────────────────────────────────────────────────
  6. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel










› Message from @a056a0c8c7f5dcc58 (ctrl+o to expand)
› Message from @a36c4450424aa863f (ctrl+o to expand)
`
	permissionScreen = `❯ Run 'touch /tmp/hookexp/x.txt'.
  Creating empty file x.txt
  ⎿  $ touch /tmp/hookexp/x.txt
────────────────────────────────────────────────
 Bash command
   touch /tmp/hookexp/x.txt
   Create empty file x.txt
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to /tmp/hookexp from this project
   3. No
 Esc to cancel · Tab to amend
`
	promptScreen = `● Asked: "Press Esc to cancel · Enter to confirm"? Sure.
  Ran 2 shell commands
  ⎿  Interrupted · What should Claude do instead?

✻ Cogitated for 5s · done 4:24 AM

────────────────────────────────────────────────
❯ 
────────────────────────────────────────────────
  ⏸ manual mode on · ← for agents
`
	waitingScreen = `● Explaining the footer "Esc to cancel · Tab to amend".
  line 1
  line 2
  line 3
  line 4
  line 5
  line 6
  line 7
  line 8
· Stewing… (esc to interrupt)
────────────────────────────────────────────────
❯ 
────────────────────────────────────────────────
  Usage limit reached · continuing shortly · esc to cancel
`
)

func TestDialogOpen(t *testing.T) {
	a := New("", nil).(adapter.DialogDetector)
	for name, tc := range map[string]struct {
		screen string
		want   bool
	}{
		"question":   {questionScreen, true},
		"permission": {permissionScreen, true},
		"prompt":     {promptScreen, false},
		"working":    {waitingScreen, false},
		"trust":      {"❯ 1. Yes, I trust this folder\n  2. No, exit\n\n Enter to confirm · Esc to cancel", true},
		"empty":      {"", false},
	} {
		if got := a.DialogOpen(tc.screen); got != tc.want {
			t.Errorf("%s: DialogOpen = %v, want %v", name, got, tc.want)
		}
	}
}

func TestHandleHookTranscript(t *testing.T) {
	const path = "/home/u/.claude/projects/-work/3f1c2a4e-7b5d-4e8a-9c1f-2d3e4f5a6b7c.jsonl"
	a := New("", nil)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Notification", "Stop", "StopFailure"} {
		u, ok := a.HandleHook(adapter.HookEvent{Event: ev, Payload: []byte(`{"session_id":"s","transcript_path":"` + path + `","tool_name":"Bash"}`)})
		if !ok || u.Transcript != path {
			t.Errorf("%s: ok %v, Transcript %q", ev, ok, u.Transcript)
		}
	}
	if u, _ := a.HandleHook(adapter.HookEvent{Event: "Stop", Payload: []byte(`{"session_id":"s"}`)}); u.Transcript != "" {
		t.Errorf("no transcript_path: Transcript %q", u.Transcript)
	}
}

func TestIdentity(t *testing.T) {
	a := New("", nil)
	if a.ID() != "claude" || a.DisplayName() == "" || !a.Capabilities().ActivityState || !a.Capabilities().InitialPrompt {
		t.Errorf("%s %q %+v", a.ID(), a.DisplayName(), a.Capabilities())
	}
}

func TestLaunchSandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the daemon user's config must stay untouched
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := filepath.Join(t.TempDir(), "sandbox-home")
	state := t.TempDir()
	// The host binary does not exist: in a sandbox the image provides it.
	a := New(filepath.Join(t.TempDir(), "missing"), nil)
	spec, err := a.Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "a", FleetBinary: "/f", StateDir: state, Sandbox: true, Home: home, TrustDir: "/work/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Argv[0] != "claude" {
		t.Errorf("argv[0] = %q, want claude", spec.Argv[0])
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
		Projects               map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.HasCompletedOnboarding || !cfg.Projects["/work/repo"].HasTrustDialogAccepted {
		t.Errorf("sandbox config = %s", b)
	}
	if fileExists(filepath.Join(os.Getenv("HOME"), ".claude.json")) {
		t.Error("the daemon user's ~/.claude.json was written")
	}

	// An existing config is kept, onboarding state included.
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600)
	if _, err := a.Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "a", FleetBinary: "/f", StateDir: state, Sandbox: true, Home: home,
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude.json")); string(b) != `{"theme":"dark"}` {
		t.Errorf("existing config changed: %s", b)
	}
}

func TestAuthFile(t *testing.T) {
	a := New("", nil).(adapter.AuthProvider)
	t.Setenv("HOME", "/h")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := a.AuthFile(""); got != "/h/.claude/.credentials.json" {
		t.Errorf("AuthFile = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	if got := a.AuthFile(""); got != "/cfg/.credentials.json" {
		t.Errorf("AuthFile with CLAUDE_CONFIG_DIR = %q", got)
	}
	if got := a.AuthFile("/sb"); got != "/sb/.claude/.credentials.json" {
		t.Errorf("AuthFile(/sb) = %q", got)
	}
	dir := t.TempDir()
	unlock, err := a.LockAuth(context.Background(), filepath.Join(dir, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dir, ".storage-write.lock")) {
		t.Error("lock dir not taken")
	}
	unlock()
	if fileExists(filepath.Join(dir, ".storage-write.lock")) {
		t.Error("lock dir not released")
	}
}
