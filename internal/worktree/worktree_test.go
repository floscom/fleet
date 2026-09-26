package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a git repo with one commit and a subdirectory.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "pkg", "sub", "f.txt"), []byte("hi\n"), 0o644)
	run(t, repo, "init", "-q", "-b", "main")
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=fleet-test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepoRoot(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	sub := filepath.Join(repo, "pkg", "sub")
	if root, ok := RepoRoot(ctx, sub); !ok || root != repo {
		t.Fatalf("RepoRoot(sub) = %q, %v", root, ok)
	}
	if _, ok := RepoRoot(ctx, t.TempDir()); ok {
		t.Fatal("temp dir reported as repo")
	}
	if !IsRepoRoot(repo) || IsRepoRoot(sub) || IsRepoRoot(t.TempDir()) {
		t.Fatal("IsRepoRoot wrong")
	}
}

func TestAddDirtyRemove(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	wt := filepath.Join(filepath.Dir(repo), "worktrees", "a1")

	if err := Add(ctx, AddOptions{Repo: filepath.Join(repo, "pkg"), Path: wt, Branch: "fleet/a1"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "fleet/a1" {
		t.Fatalf("branch = %q", got)
	}
	if !IsRepoRoot(wt) {
		t.Fatal("worktree is not a repo root")
	}
	sub, err := SubdirIn(repo, filepath.Join(repo, "pkg", "sub"), wt)
	if err != nil || sub != filepath.Join(wt, "pkg", "sub") {
		t.Fatalf("SubdirIn = %q, %v", sub, err)
	}
	if _, err := os.Stat(filepath.Join(sub, "f.txt")); err != nil {
		t.Fatal(err)
	}

	// Path must not exist.
	if err := Add(ctx, AddOptions{Repo: repo, Path: wt, Branch: "other"}); err == nil {
		t.Fatal("Add over existing path succeeded")
	}
	// Branch checked out elsewhere is a clear error.
	err = Add(ctx, AddOptions{Repo: repo, Path: wt + "-b", Branch: "fleet/a1"})
	if err == nil || !strings.Contains(err.Error(), "already checked out") {
		t.Fatalf("expected already-checked-out error, got %v", err)
	}
	if err := Add(ctx, AddOptions{Repo: repo, Path: wt + "-c", Branch: "main"}); err == nil {
		t.Fatal("checking out main in a second worktree succeeded")
	}
	if err := Add(ctx, AddOptions{Repo: repo, Path: wt + "-d", Branch: "bad..name"}); err == nil {
		t.Fatal("invalid branch accepted")
	}

	dirty, err := Dirty(ctx, wt)
	if err != nil || dirty {
		t.Fatalf("fresh worktree dirty=%v err=%v", dirty, err)
	}
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0o644)
	if dirty, _ := Dirty(ctx, wt); !dirty {
		t.Fatal("untracked file not reported dirty")
	}
	os.Remove(filepath.Join(wt, "new.txt"))
	os.WriteFile(filepath.Join(sub, "f.txt"), []byte("changed\n"), 0o644)
	if dirty, _ := Dirty(ctx, wt); !dirty {
		t.Fatal("modified file not reported dirty")
	}

	if err := Remove(ctx, wt, false); err == nil {
		t.Fatal("non-force remove of dirty worktree succeeded")
	}
	if err := Remove(ctx, wt, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree dir still exists")
	}
	if err := Remove(ctx, wt, false); err != nil {
		t.Fatalf("removing a missing worktree: %v", err)
	}
	// Branch is kept and can be checked out again in a new worktree.
	run(t, repo, "rev-parse", "--verify", "refs/heads/fleet/a1")
	wt2 := wt + "-again"
	if err := Add(ctx, AddOptions{Repo: repo, Path: wt2, Branch: "fleet/a1"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, wt2, "rev-parse", "--abbrev-ref", "HEAD"); got != "fleet/a1" {
		t.Fatalf("branch = %q", got)
	}
	if err := Remove(ctx, wt2, false); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, true); err == nil {
		t.Fatal("removed the main worktree")
	}
	if list := run(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("stale worktrees:\n%s", list)
	}
}

func TestSubdirInOutside(t *testing.T) {
	if _, err := SubdirIn("/a/repo", "/a/repo2", "/w"); err == nil {
		t.Fatal("sibling accepted")
	}
	if got, err := SubdirIn("/a/repo", "/a/repo", "/w"); err != nil || got != "/w" {
		t.Fatalf("root -> %q, %v", got, err)
	}
}

func TestSanitizeBranch(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Fix Login Bug", "fix-login-bug"},
		{"  --hello__world--  ", "hello__world"},
		{"a///b", "a-b"},
		{"v1..2", "v1.2"},
		{"a.-.b", "a-b"},
		{"a-.b", "a-b"},
		{"feature.lock", "feature"},
		{".hidden", "hidden"},
		{"Ünïcödé", "n-c-d"},
		{"", "agent"},
		{"!!!", "agent"},
		{"...", "agent"},
		{strings.Repeat("x", 100), strings.Repeat("x", 64)},
	}
	for _, tt := range tests {
		got := SanitizeBranch(tt.in)
		if got != tt.want {
			t.Errorf("SanitizeBranch(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if err := exec.Command("git", "check-ref-format", "--branch", got).Run(); err != nil {
			t.Errorf("SanitizeBranch(%q) = %q is not a valid branch", tt.in, got)
		}
	}
}
