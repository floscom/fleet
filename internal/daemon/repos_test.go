package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
)

// fakeRepos stands in for gh: clone makes a repository whose origin is
// the GitHub repository.
type fakeRepos struct {
	mu       sync.Mutex
	repos    []*fleetv1.Repo
	searches []string
	clones   []string
	cloneErr error
}

func (f *fakeRepos) login(context.Context) (string, error) { return "octo", nil }

func (f *fakeRepos) list(context.Context) ([]*fleetv1.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fleetv1.Repo
	for _, r := range f.repos {
		out = append(out, &fleetv1.Repo{FullName: r.FullName, CanPush: r.CanPush})
	}
	return out, nil
}

func (f *fakeRepos) search(_ context.Context, q string) ([]*fleetv1.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, q)
	return []*fleetv1.Repo{{FullName: "someone/" + q}}, nil
}

func (f *fakeRepos) clone(ctx context.Context, repo, path string) error {
	f.mu.Lock()
	f.clones = append(f.clones, repo)
	err := f.cloneErr
	f.mu.Unlock()
	if _, statErr := os.Lstat(path); statErr == nil {
		return errors.New("clone target exists")
	}
	if err != nil {
		// A clone cut short leaves a folder behind.
		_ = os.MkdirAll(filepath.Join(path, ".git"), 0o755)
		return err
	}
	for _, args := range [][]string{{"init", "-q", path}, {"-C", path, "remote", "add", "origin", "https://github.com/" + repo + ".git"}} {
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			return errors.New(string(out))
		}
	}
	return nil
}

func TestRepoFullName(t *testing.T) {
	for in, want := range map[string]string{
		"octo/app":                                   "octo/app",
		" octo/app.git ":                             "octo/app",
		"https://github.com/octo/app":                "octo/app",
		"https://github.com/octo/app.git":            "octo/app",
		"https://github.com/Octo/my.app/tree/main/x": "Octo/my.app",
		"https://github.com/octo/app/pull/7":         "octo/app",
		"github.com/octo/app":                        "octo/app",
		"git@github.com:octo/app.git":                "octo/app",
		"ssh://git@github.com/octo/app.git":          "octo/app",
	} {
		if got, ok := repoFullName(in); !ok || got != want {
			t.Errorf("repoFullName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "app", "-o/app", "octo/app/more", "octo/..", "octo/.",
		"https://gitlab.com/octo/app", "https://github.com/octo", "file:///tmp/x",
		"git@gitlab.com:octo/app.git", "octo/app --upload-pack=x", "../octo/app",
	} {
		if got, ok := repoFullName(in); ok {
			t.Errorf("repoFullName(%q) = %q, want refused", in, got)
		}
	}
}

func TestClonePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real, _ := filepath.EvalSymlinks(home)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, work, want string }{
		{"", "/srv/work", "/srv/work/app"},
		{"~/src/app2", "", filepath.Join(real, "src/app2")},
		{filepath.Join(link, "new/app"), "", filepath.Join(real, "new/app")},
		{"/srv/x/../y", "", "/srv/y"},
	} {
		if got, err := clonePath(c.path, c.work, "octo/app"); err != nil || got != c.want {
			t.Errorf("clonePath(%q, %q) = %q, %v; want %q", c.path, c.work, got, err, c.want)
		}
	}
	for _, c := range []struct{ path, work string }{{"rel/app", "/w"}, {"/", "/w"}, {"", ""}} {
		if got, err := clonePath(c.path, c.work, "octo/app"); err == nil {
			t.Errorf("clonePath(%q, %q) = %q, want an error", c.path, c.work, got)
		}
	}
}

func TestRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("FLEET_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	work, _ := filepath.EvalSymlinks(t.TempDir())
	fake := &fakeRepos{repos: []*fleetv1.Repo{{FullName: "octo/app", CanPush: true}, {FullName: "octo/lib"}, {FullName: "octo/elsewhere"}}}
	old := repoHub
	repoHub = fake
	t.Cleanup(func() { repoHub = old })

	d := &daemon{
		cfg:  &config.Config{WorkDir: work},
		opts: Options{Adapters: adapter.NewRegistry(testAdapter{})},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sub := &subscriber{ch: make(chan *fleetv1.Event, 16)}
	d.agents = &manager{d: d, subs: map[*subscriber]struct{}{sub: {}}}
	ctx := context.Background()

	// A clone outside the work folder that is a root is found too.
	elsewhere := filepath.Join(t.TempDir(), "other-name")
	if err := fake.clone(ctx, "octo/elsewhere", elsewhere); err != nil {
		t.Fatal(err)
	}
	elsewhere, _ = filepath.EvalSymlinks(elsewhere)
	if _, err := d.addRoot(&fleetv1.AddRootRequest{Path: elsewhere}); err != nil {
		t.Fatal(err)
	}

	// Clone into the work folder: a new, trusted root.
	res, err := d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "https://github.com/octo/app", Trust: true, Adapters: []string{"test"}})
	want := filepath.Join(work, "app")
	if err != nil || res.GetExisted() || res.GetRoot().GetPath() != want || !res.GetRoot().GetTrust() || res.GetRoot().GetName() != "app" {
		t.Fatalf("clone = %+v, %v", res, err)
	}
	if got := cloneOf(ctx, want); got != "octo/app" {
		t.Fatalf("the clone's origin is %q", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(work, ".app.clone-*")); len(leftovers) != 0 {
		t.Fatalf("temporary clone folders left: %v", leftovers)
	}
	if loaded, err := config.Load(); err != nil || len(loaded.Roots) != 2 {
		t.Fatalf("saved roots = %+v, %v", loaded, err)
	}

	// The same again: nothing is cloned, the root is the one it was.
	res, err = d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "Octo/App"})
	if err != nil || !res.GetExisted() || res.GetRoot().GetName() != "app" || len(fake.clones) != 2 || len(d.cfg.Roots) != 2 {
		t.Fatalf("again = %+v, %v (clones %v)", res, err, fake.clones)
	}

	// A clone that is no root yet becomes one.
	if err := fake.clone(ctx, "octo/lib", filepath.Join(work, "lib")); err != nil {
		t.Fatal(err)
	}
	res, err = d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "octo/lib"})
	if err != nil || !res.GetExisted() || res.GetRoot().GetPath() != filepath.Join(work, "lib") || res.GetRoot().GetTrust() {
		t.Fatalf("existing clone = %+v, %v", res, err)
	}

	// The listing says where each one is cloned.
	list, err := d.listRepos(ctx, &fleetv1.ListReposRequest{})
	if err != nil || list.GetLogin() != "octo" || list.GetWorkDir() != work || len(list.GetRepos()) != 3 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, r := range list.GetRepos() {
		want := map[string]string{"octo/app": filepath.Join(work, "app"), "octo/lib": filepath.Join(work, "lib"), "octo/elsewhere": elsewhere}[r.GetFullName()]
		if r.GetLocalPath() != want {
			t.Errorf("%s is at %q, want %q", r.GetFullName(), r.GetLocalPath(), want)
		}
	}
	if found, err := d.listRepos(ctx, &fleetv1.ListReposRequest{Query: " cli "}); err != nil || len(found.GetRepos()) != 1 || fake.searches[0] != "cli" {
		t.Fatalf("search = %+v, %v (%v)", found, err, fake.searches)
	}

	// Another folder in the way, a bad name, a failed clone.
	if err := os.MkdirAll(filepath.Join(work, "busy", "file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "octo/busy"}); codeOf(err) != codeExists {
		t.Fatalf("folder in the way: %v", err)
	}
	if _, err := d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "not a repo"}); codeOf(err) != codeInvalid {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "octo/x", Adapters: []string{"nope"}}); codeOf(err) != codeNotFound {
		t.Fatalf("unknown adapter: %v", err)
	}
	fake.cloneErr = errf(codeInvalid, "gh repo clone octo/broken: network down")
	if _, err := d.cloneRepo(ctx, &fleetv1.CloneRepoRequest{Repo: "octo/broken", Path: filepath.Join(work, "deep", "broken")}); err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("failed clone: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(work, "deep", "broken")); err == nil {
		t.Fatal("a failed clone left its folder")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(work, "deep", ".*")); len(leftovers) != 0 {
		t.Fatalf("a failed clone left %v", leftovers)
	}
	if len(d.cfg.Roots) != 3 {
		t.Fatalf("roots after the failures: %+v", d.cfg.Roots)
	}
}

// codeOf is the protocol code of err.
func codeOf(err error) fleetv1.ErrorCode {
	if err == nil {
		return fleetv1.ErrorCode_ERROR_CODE_UNSPECIFIED
	}
	c, _ := errorCode(err)
	return c
}
