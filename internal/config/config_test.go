package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "fleethome")
	t.Setenv("FLEET_HOME", h)
	return h
}

func TestHomeAndPaths(t *testing.T) {
	h := setHome(t)
	got, err := Home()
	if err != nil || got != h {
		t.Fatalf("Home() = %q, %v; want %q", got, err, h)
	}
	if _, err := os.Stat(h); !os.IsNotExist(err) {
		t.Fatalf("Home must not create the dir")
	}
	if _, err := EnsureHome(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(h)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("home mode = %v, %v", fi.Mode(), err)
	}
	if SocketPath() != filepath.Join(h, "fleet.sock") {
		t.Fatalf("SocketPath = %q", SocketPath())
	}
	if Path("agents", "x") != filepath.Join(h, "agents", "x") {
		t.Fatalf("Path = %q", Path("agents", "x"))
	}
}

func TestLoadDefaults(t *testing.T) {
	setHome(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	hn, _ := os.Hostname()
	if c.Name != hn || c.Listen != DefaultListen || c.TmuxSocket != DefaultTmuxSocket() ||
		c.DefaultIsolation != "worktree" || !c.MDNSEnabled() {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestDefaultTmuxSocket(t *testing.T) {
	u, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("FLEET_HOME", filepath.Join(u, ".fleet"))
	if got := DefaultTmuxSocket(); got != "fleet" {
		t.Fatalf("default home: %q, want fleet", got)
	}
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("FLEET_HOME", a)
	sa := DefaultTmuxSocket()
	t.Setenv("FLEET_HOME", b)
	sb := DefaultTmuxSocket()
	if sa == "fleet" || sb == "fleet" || sa == sb || !strings.HasPrefix(sa, "fleet-") {
		t.Fatalf("sockets for other homes: %q, %q", sa, sb)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	h := setHome(t)
	off := false
	dir := t.TempDir()
	c := &Config{Name: "box", Listen: "", MDNS: &off, TmuxSocket: "x", DefaultIsolation: "pinned",
		Adapters: map[string]AdapterConfig{"claude": {Binary: "/opt/claude", Args: []string{"--x"}}}}
	if _, err := c.AddRoot(Root{Path: dir, Name: "code", Adapters: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(h, "config.toml"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v", fi.Mode(), err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "box" || got.Listen != "" || got.MDNSEnabled() || got.TmuxSocket != "x" ||
		got.DefaultIsolation != "pinned" || got.Adapters["claude"].Binary != "/opt/claude" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if r, ok := got.RootByName("code"); !ok || len(r.Adapters) != 1 {
		t.Fatalf("root lost: %+v", got.Roots)
	}
	entries, _ := os.ReadDir(h)
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Fatalf("leftover file %q", e.Name())
		}
	}
}

func TestLoadBadTOML(t *testing.T) {
	h := setHome(t)
	os.MkdirAll(h, 0o700)
	os.WriteFile(filepath.Join(h, "config.toml"), []byte("name = ["), 0o600)
	if _, err := Load(); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestAddRemoveRoot(t *testing.T) {
	base := realTemp(t)
	a := mkdir(t, base, "p1", "code")
	b := mkdir(t, base, "p2", "code")
	link := filepath.Join(base, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	os.WriteFile(file, nil, 0o600)

	c := &Config{}
	r1, err := c.AddRoot(Root{Path: link})
	if err != nil || r1.Name != "code" || r1.Path != a {
		t.Fatalf("AddRoot = %+v, %v", r1, err)
	}
	r2, err := c.AddRoot(Root{Path: b})
	if err != nil || r2.Name != "code-2" {
		t.Fatalf("AddRoot second = %+v, %v", r2, err)
	}
	if _, err := c.AddRoot(Root{Path: a, Name: "other"}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if _, err := c.AddRoot(Root{Path: base, Name: "code"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := c.AddRoot(Root{Path: file}); err == nil {
		t.Fatal("file accepted as root")
	}
	if _, err := c.AddRoot(Root{Path: filepath.Join(base, "missing")}); err == nil {
		t.Fatal("missing dir accepted")
	}
	if err := c.RemoveRoot("code"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveRoot("code"); err == nil {
		t.Fatal("removing twice should fail")
	}
	if len(c.Roots) != 1 || c.Roots[0].Name != "code-2" {
		t.Fatalf("roots = %+v", c.Roots)
	}
}

func TestResolve(t *testing.T) {
	base := realTemp(t)
	code := mkdir(t, base, "code")
	sub := mkdir(t, code, "proj", "sub")
	sibling := mkdir(t, base, "code2")
	outside := mkdir(t, base, "outside")
	os.WriteFile(filepath.Join(code, "file"), nil, 0o600)
	must(t, os.Symlink(outside, filepath.Join(code, "escape")))
	must(t, os.Symlink("../code2", filepath.Join(code, "relescape")))
	must(t, os.Symlink(sub, filepath.Join(code, "inner")))
	// A root whose configured path is itself a symlink.
	must(t, os.Symlink(code, filepath.Join(base, "codelink")))

	c := &Config{Roots: []Root{
		{Name: "code", Path: code},
		{Name: "viaLink", Path: filepath.Join(base, "codelink")},
		{Name: "proj", Path: filepath.Join(code, "proj")},
	}}

	tests := []struct {
		name          string
		root, rel, ab string
		wantRoot      string
		want          string
		outside       bool
		anyErr        bool
	}{
		{name: "root itself", root: "code", rel: "", wantRoot: "code", want: code},
		{name: "dot", root: "code", rel: ".", wantRoot: "code", want: code},
		{name: "subdir", root: "code", rel: "proj/sub", wantRoot: "code", want: sub},
		{name: "inner symlink", root: "code", rel: "inner", wantRoot: "code", want: sub},
		{name: "symlinked root", root: "viaLink", rel: "proj", wantRoot: "viaLink", want: filepath.Join(code, "proj")},
		{name: "dotdot", root: "code", rel: "../outside", outside: true},
		{name: "dotdot inside", root: "code", rel: "proj/../proj", outside: true},
		{name: "abs rel", root: "code", rel: outside, outside: true},
		{name: "symlink escape", root: "code", rel: "escape", outside: true},
		{name: "relative symlink to sibling", root: "code", rel: "relescape", outside: true},
		{name: "missing", root: "code", rel: "nope", anyErr: true},
		{name: "file", root: "code", rel: "file", anyErr: true},
		{name: "unknown root", root: "zzz", rel: "", anyErr: true},
		{name: "abs in root", ab: sub, wantRoot: "proj", want: sub},
		{name: "abs root dir", ab: code, wantRoot: "code", want: code},
		{name: "abs sibling prefix", ab: sibling, outside: true},
		{name: "abs outside", ab: outside, outside: true},
		{name: "abs via escape link", ab: filepath.Join(code, "escape"), outside: true},
		{name: "abs with dotdot", ab: filepath.Join(code, "..", "outside"), outside: true},
		{name: "abs through root link", ab: filepath.Join(base, "codelink", "proj", "sub"), wantRoot: "proj", want: sub},
		{name: "not absolute", ab: "code", anyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, got, err := c.Resolve(tt.root, tt.rel, tt.ab)
			switch {
			case tt.outside:
				if !errors.Is(err, ErrOutsideRoots) {
					t.Fatalf("err = %v, want ErrOutsideRoots (got %q)", err, got)
				}
			case tt.anyErr:
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				if errors.Is(err, ErrOutsideRoots) {
					t.Fatalf("unexpected ErrOutsideRoots: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want || r.Name != tt.wantRoot {
					t.Fatalf("got (%q, %q), want (%q, %q)", r.Name, got, tt.wantRoot, tt.want)
				}
			}
		})
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		root, target string
		want         bool
	}{
		{"/a/code", "/a/code", true},
		{"/a/code", "/a/code/x", true},
		{"/a/code", "/a/code2", false},
		{"/a/code", "/a", false},
		{"/", "/anything", true},
	}
	for _, tt := range tests {
		if got := within(tt.root, tt.target); got != tt.want {
			t.Errorf("within(%q, %q) = %v", tt.root, tt.target, got)
		}
	}
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	return d
}

func mkdir(t *testing.T, elem ...string) string {
	t.Helper()
	p := filepath.Join(elem...)
	must(t, os.MkdirAll(p, 0o755))
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSandboxConfig(t *testing.T) {
	h := setHome(t)
	write := func(s string) {
		t.Helper()
		os.MkdirAll(h, 0o700)
		if err := os.WriteFile(filepath.Join(h, "config.toml"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := c.Sandbox
	if s.Default != "" || s.ImageOrDefault() != DefaultSandboxImage || s.DockerOrDefault() != "docker" ||
		s.DirOrDefault() != filepath.Join(h, "sandbox") || strings.Join(s.EnvOrDefault(), ",") != strings.Join(DefaultSandboxEnv, ",") {
		t.Fatalf("sandbox defaults wrong: %+v", s)
	}

	write("[sandbox]\ndefault = \"docker\"\nenv = []\ndir = \"~/fleet-sandbox\"\n")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	u, _ := os.UserHomeDir()
	if c.Sandbox.Default != "docker" || len(c.Sandbox.EnvOrDefault()) != 0 || c.Sandbox.DirOrDefault() != filepath.Join(u, "fleet-sandbox") {
		t.Fatalf("sandbox = %+v", c.Sandbox)
	}

	for _, bad := range []string{"[sandbox]\ndefault = \"vm\"\n", "[sandbox]\ndir = \"rel/dir\"\n"} {
		write(bad)
		if _, err := Load(); err == nil {
			t.Errorf("Load accepted %q", bad)
		}
	}

	// An empty [sandbox] table is not written back.
	write("")
	c, _ = Load()
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(h, "config.toml"))
	if strings.Contains(string(b), "sandbox") {
		t.Errorf("saved config mentions sandbox:\n%s", b)
	}
}
