package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCloneURL(t *testing.T) {
	ok := map[string]string{
		"owner/repo":                         "https://github.com/owner/repo.git",
		"owner/repo.git":                     "https://github.com/owner/repo.git",
		"github.com/owner/repo":              "https://github.com/owner/repo",
		"https://gitlab.com/g/sub/repo.git":  "https://gitlab.com/g/sub/repo.git",
		"ssh://git@host:22/repo":             "ssh://git@host:22/repo",
		"git@github.com:owner/repo.git":      "git@github.com:owner/repo.git",
		"  git@github.com:owner/repo.git\n ": "git@github.com:owner/repo.git",
	}
	for in, want := range ok {
		if got, err := NormalizeCloneURL(in); err != nil || got != want {
			t.Errorf("NormalizeCloneURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/etc", "./repo", "../repo", "file:///etc", "ext::sh -c id",
		"-uhttps://x/y", "https://", "git@host:-oProxyCommand=x", "http://github.com/o/r", "repo", "a b/c"} {
		if got, err := NormalizeCloneURL(in); err == nil {
			t.Errorf("NormalizeCloneURL(%q) = %q, want an error", in, got)
		}
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/api.git": "api",
		"git@github.com:o/web":         "web",
		"git@host:solo.git":            "solo",
		"https://host/o/r/":            "r",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloneUnpushed(t *testing.T) {
	old := cloneProtocols
	cloneProtocols = "file"
	t.Cleanup(func() { cloneProtocols = old })
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := context.Background()
	origin := newRepo(t)
	run(t, origin, "branch", "feature")
	url := "file://" + origin
	base := t.TempDir()

	// A new branch starts from the default branch.
	dir := filepath.Join(base, "clones", "a")
	if err := Clone(ctx, CloneOptions{URL: url, Path: dir, Branch: "fleet/a"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "fleet/a" {
		t.Errorf("HEAD = %s", got)
	}
	if up, err := Unpushed(ctx, dir); err != nil || up {
		t.Errorf("fresh clone Unpushed = %v, %v", up, err)
	}
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "work")
	if up, err := Unpushed(ctx, dir); err != nil || !up {
		t.Errorf("after commit Unpushed = %v, %v", up, err)
	}
	if common, err := GitCommonDir(ctx, dir); err != nil || common != filepath.Join(dir, ".git") {
		t.Errorf("GitCommonDir = %q, %v", common, err)
	}

	// A branch that exists on the remote is checked out, tracking it.
	dir = filepath.Join(base, "clones", "b")
	if err := Clone(ctx, CloneOptions{URL: url, Path: dir, Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "rev-parse", "--abbrev-ref", "feature@{upstream}"); got != "origin/feature" {
		t.Errorf("upstream = %s", got)
	}

	if err := Clone(ctx, CloneOptions{URL: url, Path: dir, Branch: "x"}); err == nil {
		t.Error("Clone into an existing path succeeded")
	}
	missing := filepath.Join(base, "clones", "c")
	if err := Clone(ctx, CloneOptions{URL: "file:///nonexistent", Path: missing, Branch: "x"}); err == nil {
		t.Error("Clone of a missing repository succeeded")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("failed clone left its directory behind")
	}
}

func TestCloneRefusesLocalTransport(t *testing.T) {
	origin := newRepo(t)
	err := Clone(context.Background(), CloneOptions{URL: "file://" + origin, Path: filepath.Join(t.TempDir(), "c"), Branch: "x"})
	if err == nil {
		t.Fatal("file:// clone allowed")
	}
}
