// Package worktree manages git worktrees for agent isolation.
package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoRoot returns the top-level directory of the git work tree containing
// dir, or ok=false if dir is not inside a git repository.
func RepoRoot(ctx context.Context, dir string) (root string, ok bool) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", false
	}
	return filepath.Clean(out), true
}

// IsRepoRoot reports whether dir is itself the top of a git work tree (or has a .git entry).
func IsRepoRoot(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	root, ok := RepoRoot(context.Background(), dir)
	if !ok {
		return false
	}
	return samePath(root, dir)
}

// AddOptions configures Add.
type AddOptions struct {
	// Repo is any directory inside the repository.
	Repo string
	// Path is where the worktree is created (must not exist).
	Path string
	// Branch is created from HEAD. If it already exists, it is checked out
	// instead (error if already checked out elsewhere).
	Branch string
}

// Add runs `git worktree add`. If the requested directory (AddOptions.Repo)
// is a subdirectory of the repo, the caller should run the agent in the
// same relative subdirectory of the new worktree - see SubdirIn.
func Add(ctx context.Context, o AddOptions) error {
	if o.Repo == "" || o.Path == "" || o.Branch == "" {
		return errors.New("worktree: Repo, Path and Branch are required")
	}
	path, err := filepath.Abs(o.Path) // git -C would resolve it against Repo
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("worktree: %s already exists", o.Path)
	}
	if _, err := git(ctx, o.Repo, "check-ref-format", "--branch", o.Branch); err != nil {
		return fmt.Errorf("worktree: invalid branch name %q", o.Branch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ref := "refs/heads/" + o.Branch
	if _, err := git(ctx, o.Repo, "rev-parse", "--verify", "--quiet", ref); err != nil {
		// New branch from HEAD.
		_, err := git(ctx, o.Repo, "worktree", "add", "-b", o.Branch, "--", path, "HEAD")
		return err
	}
	wts, err := list(ctx, o.Repo)
	if err != nil {
		return err
	}
	for _, wt := range wts {
		if wt.branch == ref {
			return fmt.Errorf("worktree: branch %q is already checked out at %s", o.Branch, wt.path)
		}
	}
	_, err = git(ctx, o.Repo, "worktree", "add", "--", path, o.Branch)
	return err
}

// SubdirIn maps dir (inside repoRoot) to the same relative location inside worktreePath.
func SubdirIn(repoRoot, dir, worktreePath string) (string, error) {
	rel, err := filepath.Rel(resolve(repoRoot), resolve(dir))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree: %s is not inside %s", dir, repoRoot)
	}
	return filepath.Join(worktreePath, rel), nil
}

// Dirty reports whether the worktree has uncommitted changes or untracked files.
func Dirty(ctx context.Context, path string) (bool, error) {
	out, err := git(ctx, path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Remove runs `git worktree remove` (with --force if force) and prunes.
// The branch is kept. Removing a path that no longer exists is not an error.
func Remove(ctx context.Context, path string, force bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	wts, err := list(ctx, path)
	if err != nil {
		return err
	}
	if len(wts) == 0 {
		return fmt.Errorf("worktree: no worktrees listed for %s", path)
	}
	main := wts[0].path // the main worktree is always listed first
	if samePath(main, path) {
		return fmt.Errorf("worktree: refusing to remove main worktree %s", path)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := git(ctx, main, append(args, "--", path)...); err != nil {
		return err
	}
	_, err = git(ctx, main, "worktree", "prune")
	return err
}

// SanitizeBranch turns an agent name into a valid branch component.
func SanitizeBranch(name string) string {
	var b []byte
	var last byte
	for _, r := range strings.ToLower(name) {
		var c byte
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			c = byte(r)
		case r == '.':
			c = '.'
		default:
			c = '-'
		}
		// Collapse runs of separators ("..", "--" and mixes like ".-.").
		if (c == '-' || c == '.') && (last == '-' || last == '.') {
			if c == '-' {
				b[len(b)-1], last = '-', '-'
			}
			continue
		}
		b = append(b, c)
		last = c
	}
	s := string(b)
	for {
		t := strings.Trim(s, "-._")
		t = strings.TrimSuffix(t, ".lock")
		if t == s {
			break
		}
		s = t
	}
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-._")
	}
	if s == "" {
		return "agent"
	}
	return s
}

type worktreeInfo struct {
	path   string
	branch string // full ref, e.g. refs/heads/main; "" when detached
}

// list parses `git worktree list --porcelain`.
func list(ctx context.Context, dir string) ([]worktreeInfo, error) {
	out, err := git(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []worktreeInfo
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			wts = append(wts, worktreeInfo{path: strings.TrimPrefix(line, "worktree ")})
		case strings.HasPrefix(line, "branch ") && len(wts) > 0:
			wts[len(wts)-1].branch = strings.TrimPrefix(line, "branch ")
		}
	}
	return wts, sc.Err()
}

// git runs git in dir and returns trimmed stdout; errors include stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// resolve returns the absolute, symlink-resolved path, or the cleaned
// absolute input if resolution fails.
func resolve(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func samePath(a, b string) bool { return resolve(a) == resolve(b) }
