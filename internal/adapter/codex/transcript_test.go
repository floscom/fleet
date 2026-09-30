package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

// Synthetic lines in the shape Codex 0.154 writes (fields the parser
// ignores mostly left out).
var ms = time.Date(2026, 9, 27, 10, 0, 0, 125e6, time.UTC).UnixMilli()

func eventLine(payload string) string {
	return `{"timestamp":"2026-09-27T10:00:00.125Z","ordinal":7,"type":"event_msg","payload":` + payload + `}`
}

func completed(item string) string {
	return eventLine(`{"type":"item_completed","thread_id":"t","turn_id":"u","item":` + item + `}`)
}

type Es = []transcript.Entry

func noteAt(text string) Es { return Es{{Kind: transcript.Note, Text: text, TimeMs: ms}} }

func tool(e transcript.Entry) Es {
	e.Kind, e.TimeMs = transcript.Tool, ms
	return Es{e}
}

func TestParseTranscript(t *testing.T) {
	tests := []struct {
		name, line string
		want       Es
	}{
		// messages
		{"user", completed(`{"type":"UserMessage","id":"1","client_id":"c","content":[{"type":"text","text":"fix the tests\n","text_elements":[]}]}`),
			Es{{Kind: transcript.User, Text: "fix the tests", TimeMs: ms}}},
		{"user with images", completed(`{"type":"UserMessage","id":"1","content":[{"type":"local_image","path":"shot.png"},{"type":"image","image_url":"data:"},{"type":"text","text":"what is this?"}]}`),
			Es{{Kind: transcript.User, Text: "[image: shot.png]\n[image]\nwhat is this?", TimeMs: ms}}},
		{"question reply", completed(`{"type":"UserMessage","id":"1","content":[{"type":"text","text":"<send_user_message_question_reply>\n[{\"answer\":\"Safari\",\"question\":\"Which browser?\",\"questionItemId\":\"x\"}]\n</send_user_message_question_reply>"}]}`),
			Es{{Kind: transcript.User, Text: "Safari", TimeMs: ms}}},
		{"empty user", completed(`{"type":"UserMessage","id":"1","content":[{"type":"text","text":"  "}]}`), nil},
		{"agent", completed(`{"type":"AgentMessage","id":"m","content":[{"type":"Text","text":"All tests pass."}],"phase":"final_answer"}`),
			Es{{Kind: transcript.Assistant, Text: "All tests pass.", TimeMs: ms}}},
		{"agent question", completed(`{"type":"AgentMessage","id":"m","content":[{"type":"Text","text":"Which browser?"}],"phase":"final_answer","delivery":"async","questions":[{"title":"Which browser?","options":null}]}`),
			Es{{Kind: transcript.Assistant, Text: "Which browser?", TimeMs: ms}}},

		// commands
		{"command", completed(`{"type":"CommandExecution","id":"exec-1","process_id":"5","command":["/bin/bash","-lc","cd web &&\nnpm test"],"cwd":"file:///w","source":"unified_exec_startup","status":"completed","stdout":"ok\n","stderr":"","aggregated_output":"ok\n","exit_code":0,"duration":{"secs":0,"nanos":5}}`),
			tool(transcript.Entry{Name: "shell", Text: "cd web &&", Detail: "cd web &&\nnpm test", Output: "ok"})},
		{"failed command", completed(`{"type":"CommandExecution","id":"exec-1","command":["/bin/bash","-lc","false"],"status":"failed","aggregated_output":"","exit_code":1}`),
			tool(transcript.Entry{Name: "shell", Text: "false", Error: true})},
		{"declined command", completed(`{"type":"CommandExecution","id":"exec-1","command":["/bin/bash","-lc","rm -rf /"],"status":"declined","exit_code":null}`),
			tool(transcript.Entry{Name: "shell", Text: "rm -rf /", Error: true})},
		{"argv command", completed(`{"type":"CommandExecution","id":"exec-1","command":["git","commit","-m","a b"],"status":"completed","stdout":"out","stderr":"err","exit_code":0}`),
			tool(transcript.Entry{Name: "shell", Text: "git commit -m 'a b'", Output: "out\nerr"})},
		{"string command", completed(`{"type":"CommandExecution","id":"exec-1","command":"ls -la","status":"completed","aggregated_output":"x","exit_code":0}`),
			tool(transcript.Entry{Name: "shell", Text: "ls -la", Output: "x"})},

		// file changes
		{"patch", completed(`{"type":"FileChange","id":"exec-2","changes":{"/w/b.md":{"type":"update","unified_diff":"@@ -1,2 +1,2 @@\n a\n-b\n+c\n","move_path":null},"/w/a.md":{"type":"add","content":"hello\nworld\n"},"/w/c.md":{"type":"delete","content":"old\n"}},"status":"completed","stdout":"Success. Updated the following files:\nA /w/a.md\nM /w/b.md\nD /w/c.md\n","stderr":""}`),
			tool(transcript.Entry{Name: "edit", Text: "/w/a.md, /w/b.md, /w/c.md", Detail: "A /w/a.md\n+hello\n+world\nM /w/b.md\n@@ -1,2 +1,2 @@\n a\n-b\n+c\nD /w/c.md"})},
		{"move", completed(`{"type":"FileChange","id":"exec-2","changes":{"/w/a.go":{"type":"update","unified_diff":"","move_path":"/w/b.go"}},"status":"completed"}`),
			tool(transcript.Entry{Name: "edit", Text: "/w/a.go", Detail: "M /w/a.go -> /w/b.go"})},
		{"failed patch", completed(`{"type":"FileChange","id":"exec-2","changes":{"/w/a.go":{"type":"update","unified_diff":"@@\n-x\n+y\n"}},"status":"failed","stdout":"","stderr":"patch did not apply\n"}`),
			tool(transcript.Entry{Name: "edit", Text: "/w/a.go", Detail: "M /w/a.go\n@@\n-x\n+y", Output: "patch did not apply", Error: true})},

		// other tools
		{"mcp", completed(`{"type":"McpToolCall","id":"exec-3","server":"docs","tool":"search","arguments":{"q":"hooks"},"status":"completed","result":{"content":[{"type":"text","text":"found 2"},{"type":"image","data":"iVBO"}],"isError":false},"duration":{"secs":0,"nanos":1}}`),
			tool(transcript.Entry{Name: "docs.search", Text: `{"q":"hooks"}`, Detail: "{\n  \"q\": \"hooks\"\n}", Output: "found 2\n[image]"})},
		{"mcp error", completed(`{"type":"McpToolCall","id":"exec-3","server":"docs","tool":"search","arguments":{},"status":"failed","result":null,"error":"server unavailable"}`),
			tool(transcript.Entry{Name: "docs.search", Output: "server unavailable", Error: true})},
		{"web search", completed(`{"type":"Extension","kind":"web.search","id":"exec-4","query":"codex hooks ...","action":{"type":"search","query":null,"queries":["codex hooks","codex hooks trust"]},"results":[{"type":"text_result","title":"Hooks","url":"https://example.com/hooks","snippet":"..."}]}`),
			tool(transcript.Entry{Name: "web_search", Text: "codex hooks ...", Detail: "codex hooks\ncodex hooks trust", Output: "Hooks https://example.com/hooks"})},
		{"open page", completed(`{"type":"Extension","kind":"web.search","id":"exec-4","query":"","action":{"type":"openPage","url":"https://example.com/"}}`),
			tool(transcript.Entry{Name: "web_search", Text: "https://example.com/"})},
		{"find in page", completed(`{"type":"Extension","kind":"web.search","id":"exec-4","query":"","action":{"type":"findInPage","url":"https://example.com/","pattern":"install"}}`),
			tool(transcript.Entry{Name: "web_search", Text: "install in https://example.com/"})},
		{"image generation", completed(`{"type":"Extension","kind":"image_gen.generation","id":"exec-5","status":"completed","revisedPrompt":"A red fox.\nPhoto.","result":"iVBORw0KGgoAAAANSUhEUg","transparentBackground":false,"failure":null,"savedPath":"/h/.codex/generated_images/s/exec-5.png"}`),
			tool(transcript.Entry{Name: "image_gen", Text: "A red fox.", Detail: "A red fox.\nPhoto.", Output: "/h/.codex/generated_images/s/exec-5.png"})},
		{"sleep", completed(`{"type":"Extension","kind":"clock.sleep","id":"call_1","durationMs":45000}`), tool(transcript.Entry{Name: "sleep", Text: "45s"})},
		{"unknown extension", completed(`{"type":"Extension","kind":"calendar.add","id":"call_1"}`), tool(transcript.Entry{Name: "calendar.add"})},
		{"view image", completed(`{"type":"ImageView","id":"exec-6","path":"file:///w/shot.png"}`), tool(transcript.Entry{Name: "view_image", Text: "/w/shot.png"})},

		// notes
		{"compaction", completed(`{"type":"ContextCompaction","id":"x"}`), noteAt("context compacted")},
		{"subagent started", completed(`{"type":"SubAgentActivity","id":"call_1","kind":"started","agent_thread_id":"t2","agent_path":"/root/server"}`), noteAt("started subagent /root/server")},
		{"subagent finished", completed(`{"type":"SubAgentActivity","id":"call_1","kind":"completed","agent_thread_id":"t2","agent_path":"/root/server"}`), noteAt("subagent /root/server finished")},
		{"subagent interacted", completed(`{"type":"SubAgentActivity","id":"call_1","kind":"interacted","agent_thread_id":"t2","agent_path":"/root/server"}`), nil},
		{"interrupted", eventLine(`{"type":"turn_aborted","turn_id":"u","reason":"interrupted","completed_at":1,"duration_ms":2}`), noteAt("interrupted")},
		{"aborted", eventLine(`{"type":"turn_aborted","turn_id":"u","reason":"replaced"}`), noteAt("turn aborted: replaced")},
		{"rolled back", eventLine(`{"type":"thread_rolled_back","num_turns":2}`), noteAt("rolled back 2 turns")},

		// skipped
		{"reasoning", completed(`{"type":"Reasoning","id":"rs_1","summary_text":["**Planning**"],"raw_content":[]}`), nil},
		{"collab", completed(`{"type":"CollabAgentToolCall","id":"call_1","tool":"wait","status":"completed"}`), nil},
		{"model's view of the prompt", `{"timestamp":"2026-09-27T10:00:00.125Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /w"}]}}`, nil},
		{"model's view of the reply", `{"timestamp":"2026-09-27T10:00:00.125Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"All tests pass."}],"phase":"final_answer"}}`, nil},
		{"function call", `{"timestamp":"2026-09-27T10:00:00.125Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"c"}}`, nil},
		{"legacy user message", eventLine(`{"type":"user_message","message":"hi","images":[]}`), nil},
		{"legacy agent message", eventLine(`{"type":"agent_message","message":"hello"}`), nil},
		{"token count", eventLine(`{"type":"token_count","info":null}`), nil},
		{"task complete", eventLine(`{"type":"task_complete","turn_id":"u","last_agent_message":"Done."}`), nil},
		{"session meta", `{"timestamp":"2026-09-27T10:00:00.125Z","type":"session_meta","payload":{"id":"s","cli_version":"0.154.0"}}`, nil},
		{"compacted", `{"timestamp":"2026-09-27T10:00:00.125Z","type":"compacted","payload":{"message":"","replacement_history":[]}}`, nil},

		// malformed
		{"empty", ``, nil},
		{"truncated", `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage"`, nil},
		{"not an object", `[]`, nil},
		{"payload not an object", `{"type":"event_msg","payload":"x"}`, nil},
		{"item not an object", eventLine(`{"type":"item_completed","item":7}`), nil},
		{"odd content", completed(`{"type":"UserMessage","content":"hi"}`), nil},
		{"odd command", completed(`{"type":"CommandExecution","command":{"x":1},"exit_code":"0"}`), tool(transcript.Entry{Name: "shell"})},
		{"odd timestamp", `{"timestamp":5,"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}`, Es{{Kind: transcript.Note, Text: "interrupted"}}},
	}
	a := New("", nil).(adapter.Transcripter)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ParseTranscript([]byte(tt.line))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestParseTranscriptClips(t *testing.T) {
	long := strings.Repeat("é", transcript.MaxText) // 2 bytes each
	lines := []string{
		completed(`{"type":"UserMessage","content":[{"type":"text","text":"` + long + `"}]}`),
		completed(`{"type":"AgentMessage","content":[{"type":"Text","text":"` + long + `"}]}`),
		completed(`{"type":"CommandExecution","command":["/bin/bash","-lc","` + long + `"],"aggregated_output":"` + long + `","exit_code":0}`),
		completed(`{"type":"FileChange","changes":{"/a":{"type":"add","content":"` + long + `"}},"status":"completed"}`),
		completed(`{"type":"McpToolCall","server":"s","tool":"t","arguments":{"q":"` + long + `"},"result":{"content":[{"type":"text","text":"` + long + `"}]}}`),
	}
	a := New("", nil).(adapter.Transcripter)
	for i, l := range lines {
		es := a.ParseTranscript([]byte(l))
		if len(es) != 1 {
			t.Fatalf("line %d: %d entries", i, len(es))
		}
		checkEntry(t, es[0])
	}
}

// checkEntry checks the limits every entry must keep.
func checkEntry(t *testing.T, e transcript.Entry) {
	t.Helper()
	const slack = 32 // Clip's "… (n more bytes)"
	for _, f := range []struct {
		name, v string
		max     int
	}{{"Text", e.Text, transcript.MaxText}, {"Detail", e.Detail, transcript.MaxDetail}, {"Output", e.Output, transcript.MaxOutput}} {
		if !utf8.ValidString(f.v) {
			t.Errorf("%s %s is not UTF-8", e.Kind, f.name)
		}
		if len(f.v) > f.max+slack {
			t.Errorf("%s %s has %d bytes", e.Kind, f.name, len(f.v))
		}
	}
	switch e.Kind {
	case transcript.Tool:
		if strings.Contains(e.Text, "\n") || len(e.Text) > 200 {
			t.Errorf("tool summary %q is not one short line", e.Text)
		}
		if e.ID != "" {
			t.Errorf("tool %s has ID %q, but no Result follows", e.Name, e.ID)
		}
	case transcript.User, transcript.Assistant, transcript.Note:
	default:
		t.Errorf("kind %q", e.Kind)
	}
}

func TestTranscriptDir(t *testing.T) {
	a := New("", nil).(adapter.Transcripter)
	t.Setenv("HOME", "/h")
	t.Setenv("CODEX_HOME", "")
	if got := a.TranscriptDir(""); got != "/h/.codex/sessions" {
		t.Errorf("TranscriptDir = %q", got)
	}
	t.Setenv("CODEX_HOME", "/ch")
	if got := a.TranscriptDir(""); got != "/ch/sessions" {
		t.Errorf("TranscriptDir with CODEX_HOME = %q", got)
	}
	if got := a.TranscriptDir("/sb"); got != "/sb/.codex/sessions" {
		t.Errorf("TranscriptDir(/sb) = %q", got)
	}
}

func TestFindTranscript(t *testing.T) {
	const sid = "01a0a1f0-90c6-7193-aae0-41d733acf4f6"
	dir := t.TempDir()
	write := func(rel string, age time.Duration) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("2026/09/14/rollout-2026-09-14T23-59-59-"+sid+".jsonl", time.Hour)
	newer := write("2026/09/15/rollout-2026-09-15T00-01-32-"+sid+".jsonl", time.Minute)
	write("2026/09/15/rollout-2026-09-15T00-03-59-01a0a1f2-cf08-77b1-8cb5-e136578e1ac4.jsonl", 0)
	write("rollout-2026-09-15T00-01-32-"+sid+".jsonl", 0) // not in a day dir

	a := New("", nil).(adapter.Transcripter)
	if got, ok := a.FindTranscript(dir, sid); !ok || got != newer {
		t.Errorf("FindTranscript = %q, %v, want %q", got, ok, newer)
	}
	for _, id := range []string{"", "missing", "*", "01a0a1f0*", "e136578e1ac4/../x", "a/b", "a?b", "[0]1", ".", strings.Repeat("a", 200)} {
		if got, ok := a.FindTranscript(dir, id); ok {
			t.Errorf("FindTranscript(%q) = %q", id, got)
		}
	}
}

// TestRealTranscripts parses every Codex rollout of the user running the
// test, to check the parser against real data after a Codex update. It
// prints nothing of their content.
func TestRealTranscripts(t *testing.T) {
	if os.Getenv("FLEET_REAL_TRANSCRIPTS") == "" {
		t.Skip("set FLEET_REAL_TRANSCRIPTS=1 to parse this machine's transcripts")
	}
	a := New("", nil).(adapter.Transcripter)
	files, _ := filepath.Glob(filepath.Join(a.TranscriptDir(""), "*", "*", "*", "rollout-*.jsonl"))
	kinds := map[string]int{}
	for _, f := range files {
		for from := int64(0); ; {
			pg, err := transcript.Forward(f, from, a.ParseTranscript)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			for _, e := range pg.Entries {
				checkEntry(t, e)
				kinds[e.Kind]++
			}
			if from = pg.End; !pg.More {
				break
			}
		}
	}
	t.Logf("%d files, entries %v", len(files), kinds)
}
