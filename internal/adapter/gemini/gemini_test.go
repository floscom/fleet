package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

func TestLaunch(t *testing.T) {
	home := t.TempDir()
	g := New("/opt/gemini", []string{"--approval-mode", "auto_edit"})
	spec, err := g.Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "a1", FleetBinary: "/usr/bin/fleet", TrustDir: "/code/api", Model: "pro",
		Prompt: "-fix it", Images: []string{"/tmp/x.png"}, ExtraArgs: []string{"--debug"},
		Sandbox: true, Home: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gemini", "--skip-trust", "--approval-mode", "auto_edit", "--debug", "--model", "pro", "--prompt-interactive=-fix it @/tmp/x.png"}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("argv = %q\nwant  %q", spec.Argv, want)
	}
	if spec.Env[envAgent] != "a1" || spec.Env[envHook] != "/usr/bin/fleet" || spec.SessionID != "" {
		t.Errorf("env %v, session %q", spec.Env, spec.SessionID)
	}

	// The extension is in the agent's home, and its hooks are the ones
	// HandleHook reads.
	ext := filepath.Join(home, ".gemini", "extensions", "fleet")
	var manifest struct{ Name string }
	if data, err := os.ReadFile(filepath.Join(ext, "gemini-extension.json")); err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Name != "fleet" {
		t.Fatalf("manifest: %s, %v", data, err)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(ext, "hooks", "hooks.json"))
	if err != nil || json.Unmarshal(data, &hooks) != nil {
		t.Fatalf("hooks: %s, %v", data, err)
	}
	for _, ev := range hookEvents {
		h := hooks.Hooks[ev]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Type != "command" || h[0].Hooks[0].Command != hookCommand(ev) || h[0].Hooks[0].Timeout != hookTimeout {
			t.Errorf("%s: %+v", ev, h)
		}
	}

	// Installing again leaves the files alone.
	fi, _ := os.Stat(filepath.Join(ext, "hooks", "hooks.json"))
	if _, err := g.Launch(context.Background(), adapter.LaunchRequest{AgentID: "a2", FleetBinary: "/f", Sandbox: true, Home: home}); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(filepath.Join(ext, "hooks", "hooks.json")); !os.SameFile(fi, fi2) {
		t.Error("extension rewritten unchanged")
	}
}

// The hook command reports to fleet in fleet's sessions and stays silent,
// reading its payload, everywhere else.
func TestHookCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fleet")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+out+"\ncat >> "+out+"\necho noise\necho noise >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) string {
		cmd := exec.Command("sh", "-c", hookCommand("AfterAgent"))
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader(`{"session_id":"s"}`)
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		if err := cmd.Run(); err != nil {
			t.Fatalf("exit: %v", err)
		}
		return b.String()
	}
	if got := run(envAgent+"=", envHook+"="+fake); got != "" {
		t.Errorf("printed %q outside fleet", got)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("reported outside fleet")
	}
	if got := run(envAgent+"=a1", envHook+"="+fake); got != "" {
		t.Errorf("printed %q", got)
	}
	if got, _ := os.ReadFile(out); string(got) != "hook --agent a1 --adapter gemini AfterAgent\n{\"session_id\":\"s\"}" {
		t.Errorf("reported %q", got)
	}
}

// Payloads as Gemini CLI 0.66.0 sends them, trimmed.
func TestHandleHook(t *testing.T) {
	g := New("", nil)
	const tp = "/h/.gemini/tmp/p/chats/session-2026-10-27T21-35-11111111.jsonl"
	for _, tc := range []struct {
		event, payload string
		ok             bool
		state          fleetv1.AgentState
		detail, tr     string
	}{
		{"SessionStart", `{"session_id":"s1","transcript_path":"` + tp + `","source":"startup"}`, true, fleetv1.AgentState_AGENT_STATE_IDLE, "", tp},
		{"SessionStart", `{"session_id":"s1","transcript_path":"/never/written.jsonl","source":"resume"}`, true, fleetv1.AgentState_AGENT_STATE_IDLE, "", ""},
		{"BeforeAgent", `{"session_id":"s1","transcript_path":"` + tp + `","prompt":"write a file"}`, true, fleetv1.AgentState_AGENT_STATE_WORKING, "", tp},
		{"Notification", `{"session_id":"s1","notification_type":"ToolPermission","message":"Tool Confirm Shell Command requires execution","details":{"type":"exec","title":"Confirm Shell Command","command":"touch c.txt","rootCommand":"touch"}}`, true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Confirm Shell Command: touch c.txt", ""},
		{"Notification", `{"session_id":"s1","notification_type":"ToolPermission","message":"Tool Confirm Write: b.txt requires editing","details":{"type":"edit","title":"Confirm Write: b.txt","fileName":"b.txt"}}`, true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Confirm Write: b.txt", ""},
		{"Notification", `{"session_id":"s1","notification_type":"ToolPermission","message":"Tool requires confirmation","details":{"type":"ask_user","title":"Ask User"}}`, true, fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, "Ask User", ""},
		{"Notification", `{"session_id":"s1","notification_type":"Other"}`, false, 0, "", ""},
		{"AfterTool", `{"session_id":"s1","tool_name":"write_file"}`, true, fleetv1.AgentState_AGENT_STATE_WORKING, "", ""},
		{"AfterAgent", `{"session_id":"s1","prompt_response":"Done."}`, true, fleetv1.AgentState_AGENT_STATE_IDLE, "", ""},
		{"SessionEnd", `{"session_id":"s1","reason":"exit"}`, false, 0, "", ""},
	} {
		u, ok := g.HandleHook(adapter.HookEvent{AgentID: "a", Event: tc.event, Payload: []byte(tc.payload)})
		if ok != tc.ok || u.State != tc.state || u.Detail != tc.detail || u.Transcript != tc.tr || (ok && u.SessionID != "s1") {
			t.Errorf("%s %s: %+v, %v", tc.event, tc.payload, u, ok)
		}
	}
}

func screen(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// trustScreen is Gemini's folder trust dialog at 120 columns.
const trustScreen = ` ╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
 │ Do you trust the files in this folder?                                                                             │
 │                                                                                                                    │
 │ Trusting a folder allows Gemini CLI to load its local configurations, including custom commands, hooks, MCP        │
 │ servers, agent skills, and settings. These configurations could execute code on your behalf or change the behavior │
 │ of the CLI.                                                                                                        │
 │                                                                                                                    │
 │ ● 1. Trust folder (proj)                                                                                           │
 │   2. Trust parent folder (lt)                                                                                      │
 │   3. Don't trust                                                                                                   │
 │                                                                                                                    │
 ╰────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
`

func TestScreens(t *testing.T) {
	g := New("", nil)
	det := g.(adapter.PromptDetector)
	dlg := g.(adapter.DialogDetector)
	approval, idle := screen(t, "approval-screen.txt"), screen(t, "idle-screen.txt")
	if d, ok := det.DetectPrompt(trustScreen); !ok || d == "" {
		t.Error("trust dialog not detected")
	}
	for _, s := range []string{approval, idle} {
		if _, ok := det.DetectPrompt(s); ok {
			t.Errorf("trust detected in\n%s", s)
		}
	}
	if !dlg.DialogOpen(approval) || !dlg.DialogOpen(trustScreen) {
		t.Error("dialog not seen")
	}
	if dlg.DialogOpen(idle) {
		t.Error("prompt seen as a dialog")
	}
}

func TestDetectLogin(t *testing.T) {
	g := New("", nil).(adapter.Loginer)
	const url = "https://accounts.google.com/o/oauth2/v2/auth?redirect_uri=https%3A%2F%2Fcodeassist.google.com%2Fauthcode&access_type=offline&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fcloud-platform&code_challenge_method=S256&code_challenge=abc&state=def&response_type=code&client_id=681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	// Printed before the TUI starts; tmux joins the wrapped URL (-J).
	codeScreen := "Please visit the following URL to authorize the application:\n\n" + url + "\n\nEnter the authorization code: \n"
	if l, ok := g.DetectLogin(codeScreen); !ok || l.URL != url || !l.Paste {
		t.Errorf("code: %+v, %v", l, ok)
	}
	chooser := ` │
 │ ? Get started
 │
 │   How would you like to authenticate for this project?
 │
 │   ● 1. Sign in with Google
 │     2. Use Gemini API Key
 │     3. Vertex AI
 │
 │   No authentication method selected.
 │   (Use Enter to select)
 │
 │   Terms of Services and Privacy Notice for Gemini CLI
 │
 │   https://geminicli.com/docs/resources/tos-privacy/
 │
 ╰──────────────────────────────────────────`
	for _, s := range []string{chooser, screen(t, "auth-screen.txt")} {
		if l, ok := g.DetectLogin(s); !ok || l.URL != "" || l.Paste {
			t.Errorf("chooser: %+v, %v", l, ok)
		}
	}
	key := " │ Enter Gemini API Key\n │ Please enter your Gemini API key. It will be securely stored in your system keychain.\n │ Paste your API key here\n │ (Press Enter to submit, Esc to cancel, Ctrl+C to clear stored key)\n"
	if l, ok := g.DetectLogin(key); !ok || !l.Paste || l.URL != "" {
		t.Errorf("API key: %+v, %v", l, ok)
	}
	for _, s := range []string{screen(t, "idle-screen.txt"), screen(t, "approval-screen.txt"), trustScreen} {
		if l, ok := g.DetectLogin(s); ok {
			t.Errorf("detected %+v", l)
		}
	}
}

func parseFile(t *testing.T, name string) []transcript.Entry {
	t.Helper()
	g := New("", nil).(adapter.Transcripter)
	var out []transcript.Entry
	for _, l := range strings.Split(strings.TrimSpace(screen(t, name)), "\n") {
		for _, e := range g.ParseTranscript([]byte(l)) {
			e.TimeMs = 0
			out = append(out, e)
		}
	}
	return out
}

func TestParseTranscript(t *testing.T) {
	// A prompt, a reply with a shell command, the final reply. The reply
	// with the command is written twice, the result again as a user record.
	got := parseFile(t, "session.jsonl")
	want := []transcript.Entry{
		{Kind: transcript.User, Text: "make c.txt"},
		{Kind: transcript.Assistant, Text: "I will create the file."},
		{Kind: transcript.Tool, Name: "Shell", Text: "touch /tmp/lt/proj/c.txt", Detail: "touch /tmp/lt/proj/c.txt", Output: "Output: (empty)"},
		{Kind: transcript.Assistant, Text: "Created c.txt. Done."},
	}
	if !equalEntries(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	// The command refused with Esc: the turn ends as interrupted.
	got = parseFile(t, "cancelled-session.jsonl")
	if n := len(got); n < 2 || got[n-1].Kind != transcript.Note || got[n-1].Text != transcript.Interrupted {
		t.Errorf("cancelled: %+v", got)
	}
	if i := slices.IndexFunc(got, func(e transcript.Entry) bool { return e.Kind == transcript.Tool }); i < 0 || !got[i].Error || !strings.Contains(got[i].Output, "denied") {
		t.Errorf("refused tool: %+v", got)
	}
}

func equalEntries(a, b []transcript.Entry) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func TestFindTranscript(t *testing.T) {
	g := New("", nil).(adapter.Transcripter)
	home := t.TempDir()
	dir := g.TranscriptDir(home)
	chats := filepath.Join(dir, "proj", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "c67dc620-4300-4887-8965-09b8141492c5"
	write := func(name, sid string) string {
		p := filepath.Join(chats, name)
		if err := os.WriteFile(p, []byte(`{"sessionId":"`+sid+`","kind":"main"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("session-2026-10-27T21-50-c67dc620.jsonl", "c67dc620-0000-0000-0000-000000000000")
	want := write("session-2026-10-27T21-54-c67dc620.jsonl", id)
	if p, ok := g.FindTranscript(dir, id); !ok || p != want {
		t.Errorf("found %q, %v", p, ok)
	}
	if _, ok := g.FindTranscript(dir, "../x"); ok {
		t.Error("found a bad id")
	}
}

func TestModelLine(t *testing.T) {
	g := New("", nil).(adapter.Modeler)
	m := adapter.SessionModel{Model: "auto"}
	g.ModelLine([]byte(`{"id":"x","type":"gemini","content":"hi","model":"gemini-2.5-pro"}`), &m)
	if m.Model != "gemini-2.5-pro" || m.Name != "gemini-2.5-pro" {
		t.Errorf("%+v", m)
	}
	g.ModelLine([]byte(`{"id":"y","type":"gemini","content":"hi","model":"gemini-9-ultra"}`), &m)
	if m.Model != "" || m.Name != "gemini-9-ultra" {
		t.Errorf("%+v", m)
	}
}
