package web

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

func TestAgentRoutesNeedAdmin(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1"}
	path := filepath.Join(t.TempDir(), "web-token")
	LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)
	for _, rt := range [][2]string{{"GET", "/api/agents"}, {"POST", "/api/agents"}, {"GET", "/api/agents/a1/chat"},
		{"GET", "/api/agents/a1/screen"}, {"POST", "/api/agents/a1/input"}, {"POST", "/api/agents/a1/stop"}} {
		var e struct{ Error string }
		if code := api(t, ts, rt[0], rt[1], "wrong", `{"adapter":"claude","root":"code","text":"hi"}`, &e); code != 401 {
			t.Fatalf("%s %s: %d %+v", rt[0], rt[1], code, e)
		}
	}
	if len(src.runs)+len(src.stops)+len(src.inputs) != 0 {
		t.Fatal("source changed without auth")
	}
}

func TestRunStopInput(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "claude-api-1", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var list struct{ Agents []Agent }
	if code := api(t, ts, "GET", "/api/agents", tok, "", &list); code != 200 || len(list.Agents) != 1 || list.Agents[0].State != "idle" {
		t.Fatalf("list: %d %+v", code, list)
	}

	var run struct{ Agent Agent }
	body := `{"adapter":"claude","root":"code","path":"/api/","prompt":"fix it","name":" x ","isolation":"pinned","sandbox":"docker"}`
	if code := api(t, ts, "POST", "/api/agents", tok, body, &run); code != 201 || run.Agent.ID != "a1" {
		t.Fatalf("run: %d %+v", code, run)
	}
	want := &fleetv1.RunAgentRequest{Adapter: "claude", Root: "code", Path: "api", Prompt: "fix it", Name: "x",
		Isolation: fleetv1.Isolation_ISOLATION_PINNED, Sandbox: fleetv1.Sandbox_SANDBOX_DOCKER}
	if len(src.runs) != 1 || src.runs[0].String() != want.String() {
		t.Fatalf("run request %v, want %v", src.runs, want)
	}
	var e struct{ Error string }
	for _, bad := range []string{`{"adapter":"claude","root":"code","isolation":"clone"}`, `{"adapter":"claude","root":"code","sandbox":"vm"}`,
		`{"root":"code"}`, `not json`} {
		if code := api(t, ts, "POST", "/api/agents", tok, bad, &e); code != 400 {
			t.Fatalf("run %s: %d %+v", bad, code, e)
		}
	}
	if code := api(t, ts, "POST", "/api/agents", tok, `{"adapter":"claude","root":"nope"}`, &e); code != 404 || !strings.Contains(e.Error, "nope") {
		t.Fatalf("run in unknown root: %d %+v", code, e)
	}

	var in struct {
		Held *bool `json:"held"`
	}
	if code := api(t, ts, "POST", "/api/agents/a1/input", tok, `{"text":"yes","submit":true,"keys":["escape","2","enter"]}`, &in); code != 200 || in.Held == nil || *in.Held {
		t.Fatalf("input: %d %+v", code, in)
	}
	if want := []fakeInput{{"a1", "yes", true, []string{"Escape", "2", "Enter"}}}; !reflect.DeepEqual(src.inputs, want) {
		t.Fatalf("inputs %+v, want %+v", src.inputs, want)
	}
	src.mu.Lock()
	src.dialog = true
	src.mu.Unlock()
	if code := api(t, ts, "POST", "/api/agents/a1/input", tok, `{"text":"blue","submit":true}`, &in); code != 200 || in.Held == nil || !*in.Held {
		t.Fatalf("input into a dialog: %d %+v", code, in)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/input", tok, `{"keys":["C-c; kill-server"]}`, &e); code != 400 {
		t.Fatalf("unknown key: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/agents/zz/input", tok, `{"text":"x"}`, &e); code != 404 {
		t.Fatalf("input to unknown agent: %d %+v", code, e)
	}

	var screen struct{ Screen string }
	if code := api(t, ts, "GET", "/api/agents/a1/screen", tok, "", &screen); code != 200 || screen.Screen != "> hello" {
		t.Fatalf("screen: %d %q", code, screen.Screen)
	}

	var stop struct {
		Agent        Agent
		WorktreeKept bool
	}
	if code := api(t, ts, "POST", "/api/agents/a1/stop", tok, `{"removeWorktree":true}`, &stop); code != 200 || stop.Agent.State != "exited" {
		t.Fatalf("stop: %d %+v", code, stop)
	}
	if len(src.stops) != 1 || src.stops[0].Agent != "a1" || !src.stops[0].RemoveWorktree || src.stops[0].Force {
		t.Fatalf("stop request %v", src.stops)
	}
}

// lineParser makes each line a note, for chat tests.
func lineParser(line []byte) []transcript.Entry {
	return []transcript.Entry{{Kind: transcript.Note, Text: string(line)}}
}

func texts(es []transcript.Entry) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.Text)
	}
	return out
}

type chatResp struct {
	Agent      Agent
	File       string
	Entries    []transcript.Entry
	Start, End int64
	Reset      bool
	More       bool
}

func TestChat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.jsonl")
	os.WriteFile(file, []byte("one\ntwo\nthr"), 0o600) // "thr" is still being written
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", UpdatedAtMs: 100, State: fleetv1.AgentState_AGENT_STATE_WORKING}
	src.chat = Chat{Path: file, Parse: lineParser}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	get := func(q string) chatResp {
		t.Helper()
		var r chatResp
		if code := api(t, ts, "GET", "/api/agents/a1/chat"+q, tok, "", &r); code != 200 {
			t.Fatalf("chat%s: %d", q, code)
		}
		return r
	}

	first := get("")
	if !first.Reset || first.File == "" || first.Start != 0 || first.End != 8 || !reflect.DeepEqual(texts(first.Entries), []string{"one", "two"}) {
		t.Fatalf("first read: %+v", first)
	}
	if first.Agent.State != "working" {
		t.Fatalf("agent: %+v", first.Agent)
	}
	if strings.Contains(first.File, "t.jsonl") {
		t.Fatalf("file key gives the path away: %q", first.File)
	}

	// Nothing new: an immediate answer without wait, or once the agent
	// changes with wait.
	q := fmt.Sprintf("?file=%s&after=%d", first.File, first.End)
	if r := get(q); len(r.Entries) != 0 || r.Reset || r.End != 8 {
		t.Fatalf("no news: %+v", r)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		src.mu.Lock()
		src.agent = &fleetv1.Agent{Id: "a1", UpdatedAtMs: 200, State: fleetv1.AgentState_AGENT_STATE_IDLE}
		src.mu.Unlock()
	}()
	start := time.Now()
	r := get(q + "&wait=1&v=100")
	if waited := time.Since(start); waited < 300*time.Millisecond || r.Agent.State != "idle" || len(r.Entries) != 0 {
		t.Fatalf("wait for agent change: after %v: %+v", waited, r)
	}

	// The line being written completes while a request waits.
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("ee\nfour\n")
		f.Close()
	}()
	r = get(q + "&wait=1&v=200")
	if !reflect.DeepEqual(texts(r.Entries), []string{"three", "four"}) || r.Start != 8 || r.End != 19 || r.Reset {
		t.Fatalf("wait for lines: %+v", r)
	}

	// Another file (a new session), or a replaced one: the latest entries
	// of the current transcript.
	if r := get("?file=other&after=19"); !r.Reset || len(r.Entries) != 4 {
		t.Fatalf("other file: %+v", r)
	}
	if r := get(fmt.Sprintf("?file=%s&after=500", first.File)); !r.Reset || len(r.Entries) != 4 {
		t.Fatalf("shrunk file: %+v", r)
	}

	// No transcript (yet), or a missing one.
	src.mu.Lock()
	src.chat = Chat{Parse: lineParser}
	src.mu.Unlock()
	if r := get(""); r.File != "" || len(r.Entries) != 0 || r.Reset {
		t.Fatalf("no transcript: %+v", r)
	}
	if r := get(q); r.File != "" || !r.Reset {
		t.Fatalf("transcript gone: %+v", r)
	}
	src.mu.Lock()
	src.chat = Chat{Path: filepath.Join(dir, "missing.jsonl"), Parse: lineParser}
	src.mu.Unlock()
	if r := get(""); r.File != "" || len(r.Entries) != 0 {
		t.Fatalf("missing transcript: %+v", r)
	}

	var e struct{ Error string }
	if code := api(t, ts, "GET", "/api/agents/zz/chat", tok, "", &e); code != 404 {
		t.Fatalf("unknown agent: %d %+v", code, e)
	}
}

func TestChatEarlier(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.jsonl")
	var b strings.Builder
	const n = 30000
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %05d %s\n", i, strings.Repeat("x", 40))
	}
	os.WriteFile(file, []byte(b.String()), 0o600)
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1"}
	src.chat = Chat{Path: file, Parse: lineParser}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	var r chatResp
	api(t, ts, "GET", "/api/agents/a1/chat", tok, "", &r)
	if r.Start == 0 || r.End != int64(b.Len()) || len(r.Entries) == 0 || len(r.Entries) == n {
		t.Fatalf("tail: start %d end %d, %d entries", r.Start, r.End, len(r.Entries))
	}
	got := texts(r.Entries)
	file0 := r.File
	for r.Start > 0 {
		var older chatResp
		api(t, ts, "GET", fmt.Sprintf("/api/agents/a1/chat?file=%s&before=%d", file0, r.Start), tok, "", &older)
		if older.Reset || older.Start >= r.Start || older.End != r.Start {
			t.Fatalf("earlier before %d: start %d end %d reset %v", r.Start, older.Start, older.End, older.Reset)
		}
		got = append(texts(older.Entries), got...)
		r.Start = older.Start
	}
	if len(got) != n || !strings.HasPrefix(got[0], "line 00000 ") || !strings.HasPrefix(got[n-1], fmt.Sprintf("line %05d ", n-1)) {
		t.Fatalf("paged back %d lines, first %q", len(got), got[0])
	}
}

func TestForwardable(t *testing.T) {
	for _, tc := range []struct {
		method, rest string
		ok           bool
	}{
		{"GET", "agents", true},
		{"POST", "agents", true},
		{"GET", "agents/a1/chat", true},
		{"GET", "agents/a1/screen", true},
		{"POST", "agents/a1/input", true},
		{"POST", "agents/a1/stop", true},
		{"GET", "agents/a1/input", false},
		{"POST", "agents/a1/chat", false},
		{"GET", "agents//chat", false},
		{"GET", "agents/a1/b/chat", false},
		{"DELETE", "agents/a1", false},
		{"GET", "roots", true},
		{"DELETE", "roots/code", true},
		{"DELETE", "roots/", false},
		{"DELETE", "roots/a/b", false},
		{"GET", "join", false},
		{"GET", "nonce", false},
	} {
		if got := forwardable(tc.method, tc.rest); got != tc.ok {
			t.Errorf("forwardable(%s %s) = %v, want %v", tc.method, tc.rest, got, tc.ok)
		}
	}
}
