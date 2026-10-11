package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

// filesParser reads lines "write <path> <ms>" and "text".
func filesParser(line []byte) []transcript.Entry {
	f := strings.Fields(string(line))
	if f[0] == "write" {
		e := transcript.Entry{Kind: transcript.Tool, Name: "Write", Files: []string{f[1]}}
		if len(f) > 2 {
			e.TimeMs = int64(len(f[2]))
		}
		return []transcript.Entry{e}
	}
	return []transcript.Entry{{Kind: transcript.Assistant, Text: f[0]}}
}

func TestFiles(t *testing.T) {
	base := t.TempDir()
	dir, out := filepath.Join(base, "work"), filepath.Join(base, "out")
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	write := func(p, s string, at time.Time) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, at, at)
	}
	write(filepath.Join(dir, "old.txt"), "old", old)
	write(filepath.Join(dir, "rel.txt"), "rel", old)
	write(filepath.Join(dir, "new.md"), "# new\n", now)
	write(filepath.Join(dir, "sub", "Report.png"), "\x89PNG\r\n\x1a\n\x00", now.Add(-time.Minute))
	write(filepath.Join(dir, "node_modules", "pkg", "x.js"), "x", now)
	write(filepath.Join(out, "plan.md"), "plan", old.Add(time.Minute))
	write(filepath.Join(out, "secret.txt"), "secret", now)
	os.Symlink(filepath.Join(out, "secret.txt"), filepath.Join(dir, "link.txt"))

	tr := filepath.Join(base, "t.jsonl")
	os.WriteFile(tr, []byte("text\nwrite "+filepath.Join(out, "plan.md")+"\nwrite rel.txt\nwrite "+filepath.Join(base, "gone.txt")+" xxxxx\n"), 0o600)
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Cwd: dir, CreatedAtMs: 1}
	src.chat = Chat{Path: tr, Parse: filesParser, StartedAtMs: now.Add(-time.Hour).UnixMilli()}
	pathTok := filepath.Join(base, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	names := func(fs []File) string {
		var s []string
		for _, f := range fs {
			n := f.Rel
			if n == "" {
				n = "/" + filepath.Base(f.Path)
			}
			if f.Written {
				n += "*"
			}
			if f.Gone {
				n += "!"
			}
			s = append(s, n)
		}
		return strings.Join(s, " ")
	}
	var r filesReply
	if code := api(t, ts, "GET", "/api/agents/a1/files", tok, "", &r); code != 200 {
		t.Fatalf("files: %d", code)
	}
	// Newest first; the gone file sorts by when it was written (5 ms).
	if got := names(r.Files); got != "new.md sub/Report.png /plan.md* rel.txt* /gone.txt*!" || r.Dir != dir || len(r.Found) != 0 || r.Partial {
		t.Fatalf("files = %q %+v", got, r)
	}
	if f := r.Files[1]; f.Size != 9 || f.Path != filepath.Join(dir, "sub", "Report.png") {
		t.Errorf("report = %+v", f)
	}

	// A search looks through the whole folder, but packages and links.
	for q, want := range map[string][2]string{
		"report":   {"sub/Report.png", ""},
		"TXT":      {"rel.txt* /gone.txt*!", "old.txt"},
		"md plan":  {"/plan.md*", ""},
		"x.js":     {"", ""},
		"  ":       {"new.md sub/Report.png /plan.md* rel.txt* /gone.txt*!", ""},
		"nothing!": {"", ""},
	} {
		r = filesReply{}
		api(t, ts, "GET", "/api/agents/a1/files?q="+strings.ReplaceAll(q, " ", "+"), tok, "", &r)
		if got := [2]string{names(r.Files), names(r.Found)}; got != want {
			t.Errorf("q=%q: %q, want %q", q, got, want)
		}
	}

	// The files written so far are remembered; newer lines are read on.
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("write old.txt\n")
	f.Close()
	r = filesReply{}
	api(t, ts, "GET", "/api/agents/a1/files?q=old", tok, "", &r)
	if got := names(r.Files) + "|" + names(r.Found); got != "old.txt*|" {
		t.Errorf("after a new line: %q", got)
	}
	// A transcript replaced is read anew.
	os.WriteFile(tr, []byte("text\n"), 0o600)
	r = filesReply{}
	api(t, ts, "GET", "/api/agents/a1/files", tok, "", &r)
	if got := names(r.Files); got != "new.md sub/Report.png" {
		t.Errorf("replaced transcript: %q", got)
	}

	// Content: of the folder, or written; nothing else.
	tr2 := filepath.Join(base, "t2.jsonl")
	os.WriteFile(tr2, []byte("write "+filepath.Join(out, "plan.md")+"\n"), 0o600)
	src.mu.Lock()
	src.chat.Path = tr2
	src.mu.Unlock()
	var fr fileReply
	if code := api(t, ts, "GET", "/api/agents/a1/file?path="+filepath.Join(dir, "sub", "Report.png"), tok, "", &fr); code != 200 {
		t.Fatalf("file: %d", code)
	}
	if fr.Type != "image/png" || fr.Text || fr.Big || string(fr.Data) != "\x89PNG\r\n\x1a\n\x00" || fr.Rel != "sub/Report.png" || fr.Size != 9 {
		t.Errorf("png = %+v", fr)
	}
	fr = fileReply{}
	api(t, ts, "GET", "/api/agents/a1/file?path="+filepath.Join(out, "plan.md"), tok, "", &fr)
	if !fr.Text || string(fr.Data) != "plan" || fr.Rel != "" {
		t.Errorf("written outside = %+v", fr)
	}
	for p, want := range map[string]int{
		filepath.Join(dir, "new.md"):                  200,
		filepath.Join(dir, "sub", "..", "old.txt"):    200,
		filepath.Join(dir, "link.txt"):                404, // leads out of the folder
		filepath.Join(out, "secret.txt"):              404,
		filepath.Join(dir, "..", "out", "secret.txt"): 404,
		filepath.Join(dir, "missing.txt"):             404,
		filepath.Join(dir, "sub"):                     400,
		"new.md":                                      400,
		"":                                            400,
	} {
		if code := api(t, ts, "GET", "/api/agents/a1/file?path="+p, tok, "", nil); code != want {
			t.Errorf("file %q: %d, want %d", p, code, want)
		}
	}
	if code := api(t, ts, "GET", "/api/agents/zz/files", tok, "", nil); code != 404 {
		t.Errorf("unknown agent: %d", code)
	}
	if code := api(t, ts, "GET", "/api/agents/a1/files", "", "", nil); code != 401 {
		t.Errorf("without the token: %d", code)
	}
}

func TestFilesNoFolder(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Cwd: "/does/not/exist"}
	pathTok := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)
	var r filesReply
	if code := api(t, ts, "GET", "/api/agents/a1/files?q=x", tok, "", &r); code != 200 || r.Dir != "" || r.Files == nil || r.Found == nil {
		t.Fatalf("no folder: %d %+v", code, r)
	}
}

func TestIsText(t *testing.T) {
	for _, tt := range []struct {
		data string
		cut  bool
		want bool
	}{
		{"hello\n", false, true},
		{"", false, true},
		{"a\x00b", false, false},
		{"\xff\xfe", false, false},
		{"grüß", false, true},
		{"gr\xc3", true, true}, // cut inside ü
		{"gr\xc3", false, false},
	} {
		if got := isText([]byte(tt.data), tt.cut); got != tt.want {
			t.Errorf("isText(%q, %v) = %v", tt.data, tt.cut, got)
		}
	}
}
