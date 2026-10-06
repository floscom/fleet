package web

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

// mediaParser reads lines "user <images>", "call <id> <text>",
// "result <id> <images>" and "text".
func mediaParser(line []byte) []transcript.Entry {
	f := strings.Fields(string(line))
	images := func(s string) []transcript.Image {
		var out []transcript.Image
		for i := range len(s) {
			out = append(out, transcript.Image{N: i, Type: "image/png"})
		}
		return out
	}
	switch f[0] {
	case "user":
		return []transcript.Entry{{Kind: transcript.User, TimeMs: 7, Images: images(f[1])}}
	case "call":
		return []transcript.Entry{{Kind: transcript.Tool, ID: f[1], Name: "Read", Text: f[2]}}
	case "result":
		return []transcript.Entry{{Kind: transcript.Result, ID: f[1], Images: images(f[2])}}
	}
	return []transcript.Entry{{Kind: transcript.Assistant, Text: f[0]}}
}

func TestMedia(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.jsonl")
	os.WriteFile(file, []byte("user xx\ncall c1 shot.png\ntext\nresult c1 x\n"), 0o600)
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1"}
	src.chat = Chat{Path: file, Parse: mediaParser, Image: func([]byte, int) (string, []byte, bool) { return "", nil, false }}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)
	key := fileKey(file)

	var m mediaReply
	if code := api(t, ts, "GET", "/api/agents/a1/media", tok, "", &m); code != 200 {
		t.Fatalf("media: %d", code)
	}
	want := []Media{
		{Image: transcript.Image{Line: 0, N: 0, Type: "image/png"}, TimeMs: 7, From: "user"},
		{Image: transcript.Image{Line: 0, N: 1, Type: "image/png"}, TimeMs: 7, From: "user"},
		{Image: transcript.Image{Line: 30, N: 0, Type: "image/png"}, From: "tool", Tool: "Read", Text: "shot.png"},
	}
	if m.File != key || !m.Reset || m.More || m.End != 42 || !reflect.DeepEqual(m.Media, want) {
		t.Fatalf("media = %+v", m)
	}

	// Newer images only; a line being written waits.
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("result c9 x\nuser x")
	f.Close()
	m = mediaReply{}
	api(t, ts, "GET", "/api/agents/a1/media?file="+key+"&after=42", tok, "", &m)
	want = []Media{{Image: transcript.Image{Line: 42, N: 0, Type: "image/png"}, From: "tool"}}
	if m.Reset || m.End != 54 || !reflect.DeepEqual(m.Media, want) {
		t.Fatalf("after = %+v", m)
	}

	// Another transcript, or one that shrank, is read from the start.
	for _, q := range []string{"file=other&after=42", "file=" + key + "&after=9999"} {
		m = mediaReply{}
		api(t, ts, "GET", "/api/agents/a1/media?"+q, tok, "", &m)
		if !m.Reset || len(m.Media) != 4 || m.End != 54 {
			t.Errorf("%s: %+v", q, m)
		}
	}

	// No transcript yet.
	src.chat.Path = ""
	m = mediaReply{}
	if code := api(t, ts, "GET", "/api/agents/a1/media?file="+key+"&after=42", tok, "", &m); code != 200 || !m.Reset || m.File != "" || m.Media == nil {
		t.Fatalf("no transcript: %d %+v", code, m)
	}
}
