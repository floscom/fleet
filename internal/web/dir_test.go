package web

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func TestDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	write := func(rel, s string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write("README.md", "hi")
	write("gone.txt", "bye")
	write("src/app/main.go", "package main")
	write("lib/a.go", "a")
	write("lib/b.go", "b")
	write("old/x.txt", "x")
	write(".gitignore", "build/\n")
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "init")
	write("README.md", "changed")
	write("lib/b.go", "changed")
	write("new.txt", "new")
	write("fresh/one.txt", "1")
	write("build/out/bin", "\x00")
	os.Remove(filepath.Join(dir, "gone.txt"))
	os.RemoveAll(filepath.Join(dir, "old"))
	os.Mkdir(filepath.Join(dir, "empty"), 0o755)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("s"), 0o644)
	os.Symlink(base, filepath.Join(dir, "up"))

	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Cwd: dir, CreatedAtMs: 1}
	pathTok := filepath.Join(base, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	list := func(p string) (dirReply, int) {
		t.Helper()
		var r dirReply
		q := ""
		if p != "" {
			q = "?" + url.Values{"path": {p}}.Encode()
		}
		code := api(t, ts, "GET", "/api/agents/a1/dir"+q, tok, "", &r)
		return r, code
	}
	show := func(r dirReply) string {
		var s []string
		for _, e := range r.Entries {
			n := e.Name
			if e.Dir {
				n += "/"
			}
			if e.Git != "" {
				n += ":" + e.Git
			}
			for _, f := range []struct {
				on   bool
				mark string
			}{{e.Changed, "*"}, {e.Empty, "0"}, {e.Gone, "!"}, {e.Link, "@"}} {
				if f.on {
					n += f.mark
				}
			}
			s = append(s, n)
		}
		return strings.Join(s, " ")
	}

	r, code := list("")
	if code != 200 || r.Path != dir || !r.Git {
		t.Fatalf("root: %d %+v", code, r)
	}
	want := "build/out/:I empty/0 fresh/:U lib/* old/:D! src/app/ up/:U@ .gitignore gone.txt:D! new.txt:U README.md:M"
	if got := show(r); got != want {
		t.Errorf("root:\n got %s\nwant %s", got, want)
	}

	// Inside a folder: paths relative to it; an ignored folder's entries
	// are ignored too.
	if r, _ := list(filepath.Join(dir, "lib")); show(r) != "a.go b.go:M" {
		t.Errorf("lib: %s", show(r))
	}
	if r, _ := list(filepath.Join(dir, "build")); show(r) != "out/:I" {
		t.Errorf("build: %s", show(r))
	}

	// Nothing outside the agent's folder, not even through a link.
	if _, code := list(filepath.Join(dir, "up")); code != 404 {
		t.Errorf("link out: %d", code)
	}
	if _, code := list(base); code != 404 {
		t.Errorf("parent: %d", code)
	}
	if _, code := list("lib"); code != 400 {
		t.Errorf("relative: %d", code)
	}
	if _, code := list(filepath.Join(dir, "nope")); code != 404 {
		t.Errorf("missing: %d", code)
	}

	// Outside git: entries without a status.
	plain := filepath.Join(base, "plain")
	os.MkdirAll(filepath.Join(plain, "d"), 0o755)
	os.WriteFile(filepath.Join(plain, "f.txt"), []byte("f"), 0o644)
	src.agent = &fleetv1.Agent{Id: "a1", Cwd: plain, CreatedAtMs: 1}
	if r, _ := list(""); r.Git || show(r) != "d/0 f.txt" {
		t.Errorf("plain: %v %s", r.Git, show(r))
	}
}
