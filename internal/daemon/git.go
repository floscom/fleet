package daemon

// Git for agents' checkouts: the daemon reads the git state of each live
// agent's checkout (worktree, clone or pinned directory) into Agent.git,
// and fetches, pulls and pushes it on request (GitRequest), so a client
// can bring an agent's work up to date and publish it beside the pull
// requests it opens (pulls.go). Git runs on the daemon host as the
// daemon's user, with their credentials, and never prompts.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
)

// gitInterval is how often the checkouts of live agents are read; a
// variable for tests.
var gitInterval = 20 * time.Second

const (
	// gitReadTimeout bounds reading a checkout's status; gitNetTimeout an
	// action that talks to the remote (hooks may run, e.g. pre-push).
	gitReadTimeout = 20 * time.Second
	gitNetTimeout  = 3 * time.Minute
	// gitOutputLines is how many of git's last lines an action returns.
	gitOutputLines = 12
)

// errNotRepo is readGit's error for a directory outside any repository.
var errNotRepo = errors.New("not a git repository")

// gitLoop reads the checkouts of live agents until ctx is cancelled.
func (m *manager) gitLoop(ctx context.Context) {
	t := time.NewTicker(gitInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var ids []string
		m.mu.Lock()
		for _, a := range m.sortedLocked() {
			if a.live() && !a.busy && a.Cwd != "" {
				ids = append(ids, a.ID)
			}
		}
		m.mu.Unlock()
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			m.refreshGit(ctx, id)
		}
	}
}

// checkoutOf returns the directory of agent a's checkout; "" if it has
// none any more.
func checkoutOf(a *agentRec) string {
	if a.Worktree != "" && a.WorktreeRemoved {
		return ""
	}
	return a.Cwd
}

// gitLock returns the lock serializing git actions on dir.
func (m *manager) gitLock(dir string) *sync.Mutex {
	m.gitMu.Lock()
	defer m.gitMu.Unlock()
	if m.gitLocks == nil {
		m.gitLocks = map[string]*sync.Mutex{}
	}
	l, ok := m.gitLocks[dir]
	if !ok {
		l = &sync.Mutex{}
		m.gitLocks[dir] = l
	}
	return l
}

// refreshGit reads agent id's checkout and stores its status, unless an
// action is working on it.
func (m *manager) refreshGit(ctx context.Context, id string) {
	m.mu.Lock()
	a, ok := m.agents[id]
	dir := ""
	if ok {
		dir = checkoutOf(a)
	}
	m.mu.Unlock()
	if dir == "" {
		return
	}
	l := m.gitLock(dir)
	if !l.TryLock() {
		return
	}
	st, err := readGit(ctx, dir)
	l.Unlock()
	m.storeGit(id, st, err)
}

// storeGit stores what reading agent id's checkout found: no status
// outside a repository, the last one with err as its detail on failure.
func (m *manager) storeGit(id string, st *fleetv1.GitStatus, err error) *fleetv1.GitStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil
	}
	old := a.Git
	switch {
	case errors.Is(err, errNotRepo):
		st = nil
	case err != nil:
		if old == nil {
			return nil
		}
		st = proto.Clone(old).(*fleetv1.GitStatus)
		st.Detail = err.Error()
		st.CheckedAtMs = time.Now().UnixMilli()
	}
	a.Git = st
	if (old == nil) != (st == nil) || st != nil && !proto.Equal(gitWithoutCheck(old), gitWithoutCheck(st)) {
		m.changedLocked(a)
	}
	if st == nil {
		return nil
	}
	return proto.Clone(st).(*fleetv1.GitStatus)
}

// gitWithoutCheck is st without the time it was read, to tell whether a
// read changed anything clients show.
func gitWithoutCheck(st *fleetv1.GitStatus) *fleetv1.GitStatus {
	c := proto.Clone(st).(*fleetv1.GitStatus)
	c.CheckedAtMs = 0
	return c
}

// gitAction runs a GitRequest and returns the checkout after it.
func (m *manager) gitAction(ctx context.Context, req *fleetv1.GitRequest) (*fleetv1.GitResponse, error) {
	m.mu.Lock()
	a, err := m.find(req.GetAgent())
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	id, name, dir, working := a.ID, a.Name, checkoutOf(a), a.State == stateWorking
	m.mu.Unlock()
	if dir == "" {
		return nil, errf(codeInvalid, "%s has no checkout any more", name)
	}
	action := req.GetAction()
	if action == fleetv1.GitAction_GIT_ACTION_PULL && working {
		return nil, errf(codeInvalid, "%s is working: pull once it is idle, as a pull changes its files", name)
	}

	l := m.gitLock(dir)
	l.Lock()
	defer l.Unlock()
	st, err := readGit(ctx, dir)
	if err != nil {
		m.storeGit(id, st, err)
		if errors.Is(err, errNotRepo) {
			return nil, errf(codeInvalid, "%s: %s is not in a git repository", name, dir)
		}
		return nil, err
	}
	var out string
	switch action {
	case fleetv1.GitAction_GIT_ACTION_UNSPECIFIED, fleetv1.GitAction_GIT_ACTION_STATUS:
	case fleetv1.GitAction_GIT_ACTION_FETCH:
		out, err = gitFetch(ctx, st)
	case fleetv1.GitAction_GIT_ACTION_PULL:
		out, err = gitPull(ctx, st, req.GetPullMode(), req.GetFromBase())
	case fleetv1.GitAction_GIT_ACTION_PUSH:
		out, err = gitPush(ctx, st, req.GetForce())
	default:
		return nil, errf(codeInvalid, "unknown git action %d", action)
	}
	if action > fleetv1.GitAction_GIT_ACTION_STATUS {
		m.d.log.Info("git", "agent", id, "action", strings.ToLower(strings.TrimPrefix(action.String(), "GIT_ACTION_")), "err", err)
		st, rerr := readGit(context.WithoutCancel(ctx), dir)
		m.storeGit(id, st, rerr)
	} else {
		m.storeGit(id, st, nil)
	}
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	var cur *fleetv1.GitStatus
	if a, ok := m.agents[id]; ok && a.Git != nil {
		cur = proto.Clone(a.Git).(*fleetv1.GitStatus)
	}
	m.mu.Unlock()
	return &fleetv1.GitResponse{Status: cur, Output: out}, nil
}

func gitFetch(ctx context.Context, st *fleetv1.GitStatus) (string, error) {
	if st.Remote == "" {
		return "", errf(codeInvalid, "the repository has no remote")
	}
	return runGit(ctx, st.Dir, gitNetTimeout, "fetch", "--quiet", st.Remote)
}

// gitPull fetches, then brings the upstream (or base) into the branch.
func gitPull(ctx context.Context, st *fleetv1.GitStatus, mode fleetv1.PullMode, fromBase bool) (string, error) {
	if st.Branch == "" {
		return "", errf(codeInvalid, "HEAD is detached: check out a branch to pull into")
	}
	if st.Operation != "" {
		return "", errf(codeInvalid, "a %s is in progress", st.Operation)
	}
	if st.Conflicts > 0 {
		return "", errf(codeInvalid, "%d files have merge conflicts", st.Conflicts)
	}
	from := st.Upstream
	if fromBase || from == "" {
		from = st.Base
	}
	if from == "" {
		return "", errf(codeInvalid, "nothing to pull from: the branch has no upstream and the remote no default branch")
	}
	fetched, err := gitFetch(ctx, st)
	if err != nil {
		return fetched, err
	}
	var args []string
	switch mode {
	case fleetv1.PullMode_PULL_MODE_UNSPECIFIED, fleetv1.PullMode_PULL_MODE_FF_ONLY:
		args = []string{"merge", "--ff-only", "--autostash", from}
	case fleetv1.PullMode_PULL_MODE_REBASE:
		args = []string{"rebase", "--autostash", from}
	case fleetv1.PullMode_PULL_MODE_MERGE:
		args = []string{"merge", "--no-edit", "--autostash", from}
	default:
		return "", errf(codeInvalid, "unknown pull mode %d", mode)
	}
	out, err := runGit(ctx, st.Dir, gitNetTimeout, args...)
	if err == nil {
		return out, nil
	}
	// Leave the checkout as it was, not half merged.
	if op := gitOperation(gitDirOf(ctx, st.Dir)); op == "rebase" || op == "merge" {
		if _, aerr := runGit(context.WithoutCancel(ctx), st.Dir, gitReadTimeout, op, "--abort"); aerr == nil {
			return "", errf(codeInvalid, "%s conflicts with %s; aborted, the checkout is as it was", strings.TrimPrefix(from, st.Remote+"/"), st.Branch)
		}
	}
	if mode != fleetv1.PullMode_PULL_MODE_REBASE && mode != fleetv1.PullMode_PULL_MODE_MERGE && strings.Contains(err.Error(), "Not possible to fast-forward") {
		return "", errf(codeInvalid, "%s and %s have diverged: pull with rebase or merge", st.Branch, from)
	}
	return "", err
}

// gitPush pushes the branch to the same name on its remote, and makes
// that its upstream if it had none or another name.
func gitPush(ctx context.Context, st *fleetv1.GitStatus, force bool) (string, error) {
	if st.Branch == "" {
		return "", errf(codeInvalid, "HEAD is detached: check out a branch to push")
	}
	if st.Remote == "" {
		return "", errf(codeInvalid, "the repository has no remote")
	}
	args := []string{"push"}
	if st.Upstream != st.Remote+"/"+st.Branch {
		args = append(args, "--set-upstream")
	}
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, st.Remote, "HEAD:refs/heads/"+st.Branch)
	out, err := runGit(ctx, st.Dir, gitNetTimeout, args...)
	if err != nil && strings.Contains(err.Error(), "[rejected]") {
		if force {
			return "", errf(codeInvalid, "%s/%s changed since the last fetch: fetch, then push again", st.Remote, st.Branch)
		}
		return "", errf(codeInvalid, "%s/%s has commits %s lacks: pull first, or force push", st.Remote, st.Branch, st.Branch)
	}
	return out, err
}

// ---------------------------------------------------------------------------
// Reading a checkout

// readGit reads the git state of the checkout dir is in.
func readGit(ctx context.Context, dir string) (*fleetv1.GitStatus, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, errf(codeInvalid, "%s is gone", dir)
	}
	paths, err := runGit(ctx, dir, gitReadTimeout, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return nil, errNotRepo
		}
		return nil, err
	}
	p := strings.Split(paths, "\n")
	if len(p) != 3 {
		return nil, errNotRepo // e.g. inside .git, or a bare repository
	}
	st := &fleetv1.GitStatus{Dir: p[0], Operation: gitOperation(p[1])}
	if fi, err := os.Stat(filepath.Join(p[2], "FETCH_HEAD")); err == nil {
		st.FetchedAtMs = fi.ModTime().UnixMilli()
	}

	status, err := runGit(ctx, st.Dir, gitReadTimeout, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	parseGitStatus(st, status)

	if log, err := runGit(ctx, st.Dir, gitReadTimeout, "log", "-1", "--format=%h%x00%ct%x00%s", "HEAD"); err == nil {
		if f := strings.SplitN(log, "\x00", 3); len(f) == 3 {
			st.Head = f[0]
			if s, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				st.HeadTimeMs = s * 1000
			}
			st.HeadSubject = f[2]
		}
	}

	if st.Branch != "" && st.Upstream != "" {
		st.Remote, _ = runGit(ctx, st.Dir, gitReadTimeout, "config", "branch."+st.Branch+".remote")
		if st.Remote == "." {
			st.Remote = "" // tracks a local branch
		}
	}
	if st.Remote == "" {
		if remotes, err := runGit(ctx, st.Dir, gitReadTimeout, "remote"); err == nil && remotes != "" {
			list := strings.Fields(remotes)
			st.Remote = list[0]
			for _, r := range list {
				if r == "origin" {
					st.Remote = r
				}
			}
		}
	}
	if st.Remote != "" {
		st.Base = gitBase(ctx, st.Dir, st.Remote)
		if st.Base == st.Upstream {
			st.Base = ""
		}
	}
	if st.Base != "" {
		if counts, err := runGit(ctx, st.Dir, gitReadTimeout, "rev-list", "--left-right", "--count", "HEAD..."+st.Base); err == nil {
			if f := strings.Fields(counts); len(f) == 2 {
				ahead, _ := strconv.Atoi(f[0])
				behind, _ := strconv.Atoi(f[1])
				st.BaseAhead, st.BaseBehind = int32(ahead), int32(behind)
			}
		}
	}
	st.CheckedAtMs = time.Now().UnixMilli()
	return st, nil
}

// parseGitStatus reads `git status --porcelain=v2 --branch -z` into st.
func parseGitStatus(st *fleetv1.GitStatus, out string) {
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case strings.HasPrefix(f, "# branch.head "):
			if b := strings.TrimPrefix(f, "# branch.head "); b != "(detached)" {
				st.Branch = b
			}
		case strings.HasPrefix(f, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(f, "# branch.upstream ")
		case strings.HasPrefix(f, "# branch.ab "):
			for _, n := range strings.Fields(strings.TrimPrefix(f, "# branch.ab ")) {
				v, _ := strconv.Atoi(n[1:])
				if n[0] == '+' {
					st.Ahead = int32(v)
				} else {
					st.Behind = int32(v)
				}
			}
		case strings.HasPrefix(f, "1 "), strings.HasPrefix(f, "? "):
			st.Changed++
		case strings.HasPrefix(f, "2 "):
			st.Changed++
			i++ // the path it was renamed or copied from
		case strings.HasPrefix(f, "u "):
			st.Changed++
			st.Conflicts++
		}
	}
}

// gitOperation names the operation in progress in git dir gitDir.
func gitOperation(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	for _, op := range []struct{ file, name string }{
		{"rebase-merge", "rebase"}, {"rebase-apply", "rebase"}, {"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, op.file)); err == nil {
			return op.name
		}
	}
	return ""
}

// gitDirOf returns the git dir of the checkout at dir; "" if unknown.
func gitDirOf(ctx context.Context, dir string) string {
	d, err := runGit(ctx, dir, gitReadTimeout, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return ""
	}
	return d
}

// gitBase returns remote's default branch ("origin/main"): what its HEAD
// points to, else main or master; "" if none.
func gitBase(ctx context.Context, dir, remote string) string {
	if b, err := runGit(ctx, dir, gitReadTimeout, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && b != "" {
		return b
	}
	for _, name := range []string{"main", "master"} {
		if _, err := runGit(ctx, dir, gitReadTimeout, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+name); err == nil {
			return remote + "/" + name
		}
	}
	return ""
}

// runGit runs git in dir and returns its output, stdout and stderr,
// trimmed to the last lines. It never prompts: no terminal (its own
// session), no stdin, GIT_TERMINAL_PROMPT=0. Optional locks are off, so
// reading the status never takes index.lock from under the agent's own
// git commands. An error carries git's message.
func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "LC_ALL=C")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimRight(out.String(), "\n")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errf(codeUnavailable, "git is not installed on this host")
		}
		if ctx.Err() != nil {
			return "", errf(codeInvalid, "git %s timed out", args[0])
		}
		msg := gitMessage(text)
		if msg == "" {
			msg = err.Error()
		}
		return "", errf(codeInvalid, "git %s: %s", args[0], msg)
	}
	if args[0] == "status" || args[0] == "log" {
		return text, nil // parsed, not shown
	}
	return lastLines(strings.TrimSpace(text), gitOutputLines), nil
}

// gitMessage is what git says on failure without its hints, shortened.
func gitMessage(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "hint:") {
			keep = append(keep, l)
		}
	}
	s = strings.Join(keep, " ")
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// cloneGit copies a git status for a client.
func cloneGit(st *fleetv1.GitStatus) *fleetv1.GitStatus {
	if st == nil {
		return nil
	}
	return proto.Clone(st).(*fleetv1.GitStatus)
}
