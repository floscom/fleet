package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/web"
)

func TestTranscriptFile(t *testing.T) {
	t.Setenv("FLEET_HOME", "/srv/fleet")
	ad := testAdapter{}
	host := &agentRec{}
	dir := ad.TranscriptDir("")
	boxed := &agentRec{Container: "fleet-x", StateDir: "/srv/sb/agents/x"}
	for _, tc := range []struct {
		a              *agentRec
		reported, want string
	}{
		{host, filepath.Join(dir, "p", "s.jsonl"), filepath.Join(dir, "p", "s.jsonl")},
		{host, filepath.Join(dir, "..", "config.toml"), ""},
		{host, "/etc/passwd", ""},
		{host, "rel/s.jsonl", ""},
		// A sandboxed agent's paths are container paths below its home.
		{boxed, "/home/fleet/.test/transcripts/s.jsonl", "/srv/sb/home/.test/transcripts/s.jsonl"},
		{boxed, "/home/fleet/.ssh/id_ed25519", ""},
		{boxed, "/home/fleet/../flo/.test/transcripts/s.jsonl", ""},
		{boxed, "/srv/sb/home/.test/transcripts/s.jsonl", ""},
	} {
		if got := transcriptFile(ad, tc.a, tc.reported); got != tc.want {
			t.Errorf("transcriptFile(%q, container %q) = %q, want %q", tc.reported, tc.a.Container, got, tc.want)
		}
	}
}

// webCall sends an admin request to the env's dashboard and decodes the
// JSON reply into out (if non-nil).
func (e *env) webCall(method, path, body string, out any) int {
	e.t.Helper()
	tok, err := web.LoadOrCreateToken(config.Path("web-token"))
	if err != nil {
		e.t.Fatal(err)
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://"+e.web+path, r)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// chatTexts returns the texts of an agent's chat entries, and the file key.
func (e *env) chatTexts(agent string) ([]string, string) {
	e.t.Helper()
	var r struct {
		File    string
		Entries []struct{ Text string }
	}
	if code := e.webCall("GET", "/api/agents/"+agent+"/chat", "", &r); code != 200 {
		e.t.Fatalf("chat: %d", code)
	}
	var out []string
	for _, en := range r.Entries {
		out = append(out, en.Text)
	}
	return out, r.File
}

func TestChatTranscript(t *testing.T) {
	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code"})
	dir := testAdapter{}.TranscriptDir("")
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	check := func(what string, want ...string) {
		t.Helper()
		got, file := e.chatTexts(a.GetId())
		if strings.Join(got, "|") != strings.Join(want, "|") || (file == "") != (len(want) == 0) {
			t.Fatalf("%s: %q (file %q), want %q", what, got, file, want)
		}
	}

	check("no transcript yet")
	// Found by session id, and remembered.
	os.WriteFile(filepath.Join(dir, a.GetSessionId()+".jsonl"), []byte("hi\n"), 0o600)
	check("by session id", "hi")
	// Reported by a hook: wins (a new session after /clear).
	hook := func(path string) {
		c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
			AgentId: a.GetId(), Adapter: "test", Event: "transcript", Payload: []byte(path),
		}}})
	}
	reported := filepath.Join(dir, "p", "next.jsonl")
	os.WriteFile(reported, []byte("again\nand again\n"), 0o600)
	hook(reported)
	check("reported", "again", "and again")
	// Paths outside the transcript dir are ignored...
	hook(filepath.Join(e.home, "config.toml"))
	check("outside path", "again", "and again")
	// ...and so is a symlink that leads out of it.
	link := filepath.Join(dir, "p", "link.jsonl")
	os.Symlink(filepath.Join(e.home, "config.toml"), link)
	hook(link)
	check("symlink out of the transcript dir")
	// It survives a daemon restart.
	hook(reported)
	e.stop()
	e.start()
	check("after restart", "again", "and again")
}

func TestWebInputAndScreen(t *testing.T) {
	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", ExtraArgs: []string{"cat"}})
	path := "/api/agents/" + a.GetId()
	var in struct{ Held bool }
	if code := e.webCall("POST", path+"/input", `{"text":"typed","submit":true}`, &in); code != 200 || in.Held {
		t.Fatalf("input: %d %+v", code, in)
	}
	// Several lines arrive as one paste, then a key.
	if code := e.webCall("POST", path+"/input", `{"text":"line one\nline two","keys":["enter"]}`, nil); code != 200 {
		t.Fatalf("paste: %d", code)
	}
	var screen struct{ Screen string }
	waitFor(t, "input on the screen", func() bool {
		e.webCall("GET", path+"/screen", "", &screen)
		return strings.Count(screen.Screen, "typed") == 2 && strings.Count(screen.Screen, "line two") == 2
	})

	var stop struct{ Agent web.Agent }
	if code := e.webCall("POST", path+"/stop", `{}`, &stop); code != 200 || stop.Agent.State != "exited" {
		t.Fatalf("stop: %d %+v", code, stop)
	}
	var fail struct{ Error string }
	if code := e.webCall("GET", path+"/screen", "", &fail); code != 400 || !strings.Contains(fail.Error, "not running") {
		t.Fatalf("screen of a finished agent: %d %+v", code, fail)
	}
	if code := e.webCall("POST", path+"/input", `{"text":"x"}`, &fail); code != 400 {
		t.Fatalf("input to a finished agent: %d %+v", code, fail)
	}
}

// A dialog a hook reported stays until its own call moves on, or the screen
// no longer shows it; Enter is not pressed after text typed into it.
func TestWebInputDialog(t *testing.T) {
	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code",
		ExtraArgs: []string{"printf '" + testQuestion + "\\n'; read x; clear; cat"}})
	path := "/api/agents/" + a.GetId()
	hook := func(event, payload string) {
		t.Helper()
		c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
			AgentId: a.GetId(), Adapter: "test", Event: event, Payload: []byte(payload),
		}}})
	}
	var screen struct{ Screen string }
	waitFor(t, "the question on the screen", func() bool {
		e.webCall("GET", path+"/screen", "", &screen)
		return strings.Contains(screen.Screen, testQuestion)
	})
	hook("working", "")
	hook("ask", "/color")
	asking := func(a *fleetv1.Agent) bool {
		return a.GetState() == stateNeedsInput && a.GetStateDetail() == "asks color"
	}
	c.waitAgent(a.GetId(), "asking", asking)
	// Other calls, of a subagent or the agent itself, go on meanwhile.
	hook("done", "sub1/ls")
	hook("done", "/launch")
	var in struct{ Held bool }
	if code := e.webCall("POST", path+"/input", `{"text":"blue","submit":true}`, &in); code != 200 || !in.Held {
		t.Fatalf("input into the dialog: %d %+v", code, in)
	}
	time.Sleep(3 * reconcileInterval)
	if got := c.agent(a.GetId()); !asking(got) {
		t.Fatalf("the dialog closed early: %v", got)
	}
	e.webCall("GET", path+"/screen", "", &screen)
	if strings.Count(screen.Screen, "blue") != 1 {
		t.Fatalf("Enter was pressed after the text:\n%s", screen.Screen)
	}
	// Answered without a hook: gone from the screen, so closed.
	if code := e.webCall("POST", path+"/input", `{"keys":["enter"]}`, nil); code != 200 {
		t.Fatalf("enter: %d", code)
	}
	c.waitAgent(a.GetId(), "back to work", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })
	if code := e.webCall("POST", path+"/input", `{"text":"typed","submit":true}`, &in); code != 200 || in.Held {
		t.Fatalf("input without a dialog: %d %+v", code, in)
	}
	waitFor(t, "the line echoed", func() bool {
		e.webCall("GET", path+"/screen", "", &screen)
		return strings.Count(screen.Screen, "typed") == 2
	})
	// A dialog closes on news of its own call.
	hook("ask", "sub1/rm")
	c.waitAgent(a.GetId(), "asking again", func(a *fleetv1.Agent) bool { return a.GetState() == stateNeedsInput })
	hook("done", "sub1/rm")
	c.waitAgent(a.GetId(), "working again", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })
}

func TestWebRunAgent(t *testing.T) {
	e := newWebEnv(t)
	var run struct{ Agent web.Agent }
	if code := e.webCall("POST", "/api/agents", `{"adapter":"test","root":"code","name":"from-web"}`, &run); code != 201 || run.Agent.State != "running" {
		t.Fatalf("run: %d %+v", code, run)
	}
	var list struct{ Agents []web.Agent }
	if code := e.webCall("GET", "/api/agents", "", &list); code != 200 || len(list.Agents) != 1 || list.Agents[0].Name != "from-web" {
		t.Fatalf("list: %d %+v", code, list)
	}
	var fail struct{ Error string }
	for body, want := range map[string]int{
		`{"adapter":"test","root":"code","name":"from-web"}`: 400, // name taken
		`{"adapter":"test","root":"nope"}`:                   404,
		`{"adapter":"missing","root":"code"}`:                503,
		`{"adapter":"test","root":"code","path":"../x"}`:     400,
	} {
		if code := e.webCall("POST", "/api/agents", body, &fail); code != want || fail.Error == "" {
			t.Errorf("run %s: %d %+v, want %d", body, code, fail, want)
		}
	}
}
