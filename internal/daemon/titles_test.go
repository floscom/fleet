package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

func TestPromptTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	tests := []struct{ in, want string }{
		{"fix the tests", "fix the tests"},
		{"  Fix the flaky checkout test.  ", "Fix the flaky checkout test"},
		{"When I have a session open in the dashboard, don't send me a push for it. I'm already looking at it.",
			"When I have a session open in the dashboard, don't send me a push for it"},
		{"Hi. Can you look at the failing build please", "Hi. Can you look at the failing build please"},
		{"[Image #1] what is wrong with this screen?", "what is wrong with this screen?"},
		{"\n\nVideo: /srv/uploads/clip.mp4\nlook at   this clip", "look at this clip"},
		{"Video: /srv/uploads/clip.mp4", ""},
		{"PDF: /srv/uploads/spec.pdf\nsummarise it", "summarise it"},
		{"File: /srv/uploads/logs.zip\nwhat failed here", "what failed here"},
		{"/model opus", ""},
		{"", ""},
		{long, strings.TrimSpace(strings.Repeat("word ", 16)) + "…"},
		{strings.Repeat("ä", 100), strings.Repeat("ä", titleMax) + "…"},
	}
	for _, tt := range tests {
		if got := promptTitle(tt.in); got != tt.want {
			t.Errorf("promptTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplyTitle(t *testing.T) {
	a := &agentRec{}
	var f titleFind
	f.read([]transcript.Entry{{Kind: transcript.User, Text: "/model opus"}, {Kind: transcript.User, Text: "Fix the tests."}, {Kind: transcript.User, Text: "and the docs"}})
	if !a.applyTitle(f) || a.Title != "Fix the tests" || !a.TitleFromPrompt {
		t.Fatalf("from the first message: %+v", a)
	}
	if a.applyTitle(titleFind{first: "something later"}) || a.Title != "Fix the tests" {
		t.Fatalf("a later message replaced it: %+v", a)
	}
	f = titleFind{}
	f.read([]transcript.Entry{{Kind: transcript.Title, Text: "Flaky tests"}, {Kind: transcript.Title, Text: " Flaky checkout tests "}})
	if !a.applyTitle(f) || a.Title != "Flaky checkout tests" || a.TitleFromPrompt {
		t.Fatalf("the CLI's latest title: %+v", a)
	}
	if a.applyTitle(f) {
		t.Fatal("the same title again counts as a change")
	}
	if a.applyTitle(titleFind{first: "Fix the tests"}) || a.Title != "Flaky checkout tests" {
		t.Fatalf("a message replaced the CLI's title: %+v", a)
	}
}

// TestTitleBackfill reads the title of a transcript bigger than one
// forward read: the first message from its start, the latest title from
// its end.
func TestTitleBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	b.WriteString("user Port the parser to Go. Keep the tests.\ntitle Parser port\n")
	pad := strings.Repeat("x", 1000)
	for b.Len() < 6<<20 {
		b.WriteString("note " + pad + "\n")
	}
	b.WriteString("title Go parser port\nuser and now the docs\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, n := titleBackfill(path, pullAdapter{}.ParseTranscript)
	if f.first != "Port the parser to Go" || f.cli != "Go parser port" {
		t.Fatalf("backfill: %+v", f)
	}
	if n <= 0 || n > 6<<20 {
		t.Fatalf("read %d bytes", n)
	}
}

func TestSessionTitles(t *testing.T) {
	interval := pullInterval
	pullInterval = 50 * time.Millisecond
	t.Cleanup(func() { pullInterval = interval })

	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Adapter: "pulls", Root: "code", ExtraArgs: []string{"cat"}})
	id := a.GetId()
	file := filepath.Join(testAdapter{}.TranscriptDir(""), a.GetSessionId()+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(lines ...string) {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			fmt.Fprintln(f, l)
		}
		f.Close()
	}

	write("user /model opus", "user Fix the flaky checkout test. It fails one run in five.")
	got := c.waitAgent(id, "a title from the first message", func(a *fleetv1.Agent) bool { return a.GetTitle() != "" })
	if got.GetTitle() != "Fix the flaky checkout test" {
		t.Fatalf("title %q", got.GetTitle())
	}
	updated := got.GetUpdatedAtMs()

	write("user and the cart test", "title Flaky checkout test")
	got = c.waitAgent(id, "the CLI's title", func(a *fleetv1.Agent) bool { return a.GetTitle() == "Flaky checkout test" })
	if got.GetUpdatedAtMs() != updated {
		t.Fatalf("a title moved updated_at_ms: %d, was %d", got.GetUpdatedAtMs(), updated)
	}

	var list struct{ Agents []struct{ ID, Title string } }
	if code := e.webCall("GET", "/api/agents", "", &list); code != 200 {
		t.Fatalf("GET /api/agents: %d", code)
	}
	for _, x := range list.Agents {
		if x.ID == id && x.Title != "Flaky checkout test" {
			t.Fatalf("web title %q", x.Title)
		}
	}
}
