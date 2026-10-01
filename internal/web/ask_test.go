package web

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/ask"
	"fleet/internal/transcript"
)

func (s *fakeSource) Answer(agent, id string, a ask.Answer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || agent != s.agent.Id || len(s.chat.Asks) == 0 || s.chat.Asks[0].ID != id {
		return &Error{Status: 404, Msg: "the question was answered or is gone"}
	}
	s.answers = append(s.answers, a)
	s.chat.Asks = nil
	return nil
}

func TestAnswer(t *testing.T) {
	dir := t.TempDir()
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", UpdatedAtMs: 100, State: fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT}
	qs := []ask.Question{{Question: "Which color?", Header: "Color", Options: []ask.Option{{Label: "Red"}, {Label: "Blue"}}}}
	src.chat = Chat{Asks: []ask.Pending{{ID: "q1", Questions: qs}}}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	var r struct{ Asks []ask.Pending }
	if code := api(t, ts, "GET", "/api/agents/a1/chat", tok, "", &r); code != 200 || !reflect.DeepEqual(r.Asks, src.chat.Asks) {
		t.Fatalf("chat: %d %+v", code, r)
	}

	// The questions changing is news for a waiting request that names the
	// ones it knows; one that names none only waits for the agent.
	go func() {
		time.Sleep(300 * time.Millisecond)
		src.mu.Lock()
		src.chat.Asks = append(src.chat.Asks, ask.Pending{ID: "q2", Questions: qs})
		src.mu.Unlock()
	}()
	start := time.Now()
	if code := api(t, ts, "GET", "/api/agents/a1/chat?file=&after=0&wait=1&v=100&asks=q1", tok, "", &r); code != 200 || len(r.Asks) != 2 {
		t.Fatalf("wait for questions: %d %+v", code, r)
	}
	if waited := time.Since(start); waited < 200*time.Millisecond || waited > chatWait/2 {
		t.Fatalf("waited %v", waited)
	}
	src.mu.Lock()
	src.chat.Asks = src.chat.Asks[:1]
	src.mu.Unlock()

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/answer", tok, `{"answers":{}}`, &e); code != 400 {
		t.Fatalf("no ask: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/answer", tok, `{"ask":"zz","answers":{"Which color?":["Red"]}}`, &e); code != 404 {
		t.Fatalf("unknown ask: %d %+v", code, e)
	}
	body := `{"ask":"q1","answers":{"Which color?":["Teal"]},"notes":{"Which color?":"any teal"}}`
	if code := api(t, ts, "POST", "/api/agents/a1/answer", tok, body, &e); code != 200 {
		t.Fatalf("answer: %d %+v", code, e)
	}
	want := ask.Answer{Answers: map[string][]string{"Which color?": {"Teal"}}, Notes: map[string]string{"Which color?": "any teal"}}
	if len(src.answers) != 1 || !reflect.DeepEqual(src.answers[0], want) {
		t.Fatalf("answers = %+v", src.answers)
	}
}

func TestImage(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.jsonl")
	// Line i holds images "<i>:<n>" for n < i; a line still being written
	// follows.
	os.WriteFile(file, []byte("0\n1\n2\n3"), 0o600)
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1"}
	src.chat = Chat{Path: file, Parse: lineParser, Image: func(line []byte, n int) (string, []byte, bool) {
		var i int
		if _, err := fmt.Sscan(string(line), &i); err != nil || n >= i {
			return "", nil, false
		}
		typ := "image/png"
		if i == 2 {
			typ = "image/svg+xml" // not shown: may carry script
		}
		return typ, []byte(fmt.Sprintf("%d:%d", i, n)), true
	}}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)
	key := fileKey(file)

	var img struct {
		Type string
		Data []byte
	}
	if code := api(t, ts, "GET", "/api/agents/a1/image?file="+key+"&line=2&n=0", tok, "", &img); code != 200 || img.Type != "image/png" || !bytes.Equal(img.Data, []byte("1:0")) {
		t.Fatalf("image: %d %+v", code, img)
	}
	for _, q := range []string{
		"file=" + key + "&line=2&n=1",  // no such image in the line
		"file=" + key + "&line=1&n=0",  // not the start of a line
		"file=" + key + "&line=4&n=0",  // svg
		"file=" + key + "&line=6&n=0",  // a line not complete yet
		"file=" + key + "&line=99&n=0", // past the end
		"file=other&line=2&n=0",        // another transcript
	} {
		var e struct{ Error string }
		if code := api(t, ts, "GET", "/api/agents/a1/image?"+q, tok, "", &e); code != 404 {
			t.Errorf("%s: %d %+v", q, code, e)
		}
	}
	var e struct{ Error string }
	if code := api(t, ts, "GET", "/api/agents/a1/image?file="+key, tok, "", &e); code != 400 || !strings.Contains(e.Error, "line") {
		t.Errorf("no line: %d %+v", code, e)
	}

	// Entries carry the offset of the line their images are in.
	pg, err := transcript.Forward(file, 0, func(line []byte) []transcript.Entry {
		return []transcript.Entry{{Kind: transcript.User, Images: []transcript.Image{{N: 0}}}}
	})
	if err != nil || len(pg.Entries) != 3 || pg.Entries[2].Images[0].Line != 4 {
		t.Fatalf("entries: %+v %v", pg.Entries, err)
	}
}
