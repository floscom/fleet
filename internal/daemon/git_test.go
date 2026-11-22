package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

// gitT runs git in dir for a test and returns its trimmed output.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitT writes file name in dir and commits it.
func commitT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-qm", "change "+name)
}

func TestGitActions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	cfg := filepath.Join(dir, "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// A remote with main, the agent's clone on its own branch, and
	// someone else's clone.
	origin := filepath.Join(dir, "origin.git")
	gitT(t, dir, "init", "-q", "--bare", origin)
	seed := filepath.Join(dir, "seed")
	gitT(t, dir, "clone", "-q", origin, seed)
	commitT(t, seed, "a.txt", "one\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	work := filepath.Join(dir, "work")
	gitT(t, dir, "clone", "-q", origin, work)
	gitT(t, work, "switch", "-qc", "fleet/x")
	commitT(t, work, "b.txt", "agent\n")

	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{Adapters: adapter.NewRegistry(testAdapter{})}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	m.agents["a1"] = &agentRec{ID: "a1", Name: "claude-x-1", Adapter: "test", State: stateIdle, Cwd: filepath.Join(work)}

	ctx := context.Background()
	do := func(req *fleetv1.GitRequest) *fleetv1.GitStatus {
		t.Helper()
		req.Agent = "claude-x-1"
		r, err := m.gitAction(ctx, req)
		if err != nil {
			t.Fatalf("%v: %v", req, err)
		}
		if got := m.agents["a1"].proto().GetGit(); got.GetHead() != r.Status.GetHead() {
			t.Fatalf("agent shows %v, action returned %v", got, r.Status)
		}
		return r.Status
	}
	fails := func(req *fleetv1.GitRequest, want string) {
		t.Helper()
		req.Agent = "a1"
		_, err := m.gitAction(ctx, req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v: error %v, want %q", req, err, want)
		}
	}
	pull := func(mode fleetv1.PullMode, fromBase bool) *fleetv1.GitRequest {
		return &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_PULL, PullMode: mode, FromBase: fromBase}
	}
	push := func(force bool) *fleetv1.GitRequest {
		return &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_PUSH, Force: force}
	}

	// A new branch: nothing upstream, one commit ahead of main.
	st := do(&fleetv1.GitRequest{})
	if st.Branch != "fleet/x" || st.Upstream != "" || st.Remote != "origin" || st.Base != "origin/main" ||
		st.BaseAhead != 1 || st.BaseBehind != 0 || st.Changed != 0 || st.HeadSubject != "change b.txt" || st.Dir != work {
		t.Fatalf("new branch: %v", st)
	}

	// Push makes origin/fleet/x its upstream.
	st = do(push(false))
	if st.Upstream != "origin/fleet/x" || st.Ahead != 0 || st.Behind != 0 || st.Base != "origin/main" {
		t.Fatalf("pushed: %v", st)
	}
	if got := gitT(t, origin, "rev-parse", "fleet/x"); got != gitT(t, work, "rev-parse", "HEAD") {
		t.Fatalf("origin has fleet/x at %s", got)
	}

	// main moves on: a fetch shows it, a fast-forward pull from it fails
	// (diverged), a rebase brings it in, and the push then needs force.
	commitT(t, seed, "c.txt", "other\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	st = do(&fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_FETCH})
	if st.BaseBehind != 1 || st.FetchedAtMs == 0 {
		t.Fatalf("fetched: %v", st)
	}
	fails(pull(fleetv1.PullMode_PULL_MODE_UNSPECIFIED, true), "diverged")
	os.WriteFile(filepath.Join(work, "notes.txt"), []byte("wip\n"), 0o644) // untracked: kept
	st = do(pull(fleetv1.PullMode_PULL_MODE_REBASE, true))
	if st.BaseBehind != 0 || st.BaseAhead != 1 || st.Ahead != 2 || st.Behind != 1 || st.Changed != 1 {
		t.Fatalf("rebased: %v", st)
	}
	fails(push(false), "pull first, or force push")
	st = do(push(true))
	if st.Ahead != 0 || st.Behind != 0 {
		t.Fatalf("force pushed: %v", st)
	}

	// Someone else pushes to fleet/x: a pull fast-forwards to it.
	gitT(t, seed, "fetch", "-q")
	gitT(t, seed, "switch", "-q", "fleet/x")
	commitT(t, seed, "d.txt", "review fix\n")
	gitT(t, seed, "push", "-q", "origin", "fleet/x")
	st = do(pull(fleetv1.PullMode_PULL_MODE_FF_ONLY, false))
	if st.Ahead != 0 || st.Behind != 0 || st.HeadSubject != "change d.txt" {
		t.Fatalf("pulled: %v", st)
	}

	// A merge that conflicts is aborted.
	gitT(t, seed, "switch", "-q", "main")
	commitT(t, seed, "b.txt", "theirs\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	head := gitT(t, work, "rev-parse", "HEAD")
	fails(pull(fleetv1.PullMode_PULL_MODE_MERGE, true), "aborted")
	if got := gitT(t, work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
	if st := m.agents["a1"].Git; st.Operation != "" || st.Conflicts != 0 || st.BaseBehind != 1 {
		t.Fatalf("after the aborted merge: %v", st)
	}

	// No pull while the agent works; detached HEAD pushes nothing.
	m.agents["a1"].State = stateWorking
	fails(pull(fleetv1.PullMode_PULL_MODE_FF_ONLY, false), "is working")
	m.agents["a1"].State = stateIdle
	gitT(t, work, "switch", "-q", "--detach")
	fails(push(false), "detached")
	gitT(t, work, "switch", "-q", "fleet/x")

	// Outside a repository there is no status.
	plain := filepath.Join(dir, "plain")
	os.Mkdir(plain, 0o755)
	m.agents["a1"].Cwd = plain
	fails(&fleetv1.GitRequest{}, "not in a git repository")
	if m.agents["a1"].Git != nil {
		t.Fatalf("status outside a repository: %v", m.agents["a1"].Git)
	}
}

func TestParseGitStatus(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 0123", "# branch.head feat", "# branch.upstream origin/feat", "# branch.ab +3 -2",
		"1 .M N... 100644 100644 100644 aa bb x.go",
		"2 R. N... 100644 100644 100644 aa bb R100 new.go", "old.go",
		"u UU N... 100644 100644 100644 100644 aa bb cc c.go",
		"? new.txt", "",
	}, "\x00")
	var st fleetv1.GitStatus
	parseGitStatus(&st, out)
	if st.Branch != "feat" || st.Upstream != "origin/feat" || st.Ahead != 3 || st.Behind != 2 || st.Changed != 4 || st.Conflicts != 1 {
		t.Fatalf("parsed %v", &st)
	}
	st = fleetv1.GitStatus{}
	parseGitStatus(&st, "# branch.oid 0123\x00# branch.head (detached)\x00")
	if st.Branch != "" {
		t.Fatalf("detached: %v", &st)
	}
}

// TestGitSafety covers what a pull or push must not do on a checkout
// pinned to the default branch, and the status of worktrees.
func TestGitSafety(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	cfg := filepath.Join(dir, "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	origin := filepath.Join(dir, "origin.git")
	gitT(t, dir, "init", "-q", "--bare", origin)
	seed := filepath.Join(dir, "seed")
	gitT(t, dir, "clone", "-q", origin, seed)
	commitT(t, seed, "a.txt", "one\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	work := filepath.Join(dir, "work")
	gitT(t, dir, "clone", "-q", origin, work)
	os.Mkdir(filepath.Join(work, "sub"), 0o755)

	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{Adapters: adapter.NewRegistry(testAdapter{})}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	// Two agents pinned to the same checkout, one in a subfolder.
	m.agents["a1"] = &agentRec{ID: "a1", Name: "one", Adapter: "test", State: stateIdle, Cwd: work, UpdatedAtMs: 1}
	m.agents["a2"] = &agentRec{ID: "a2", Name: "two", Adapter: "test", State: stateIdle, Cwd: filepath.Join(work, "sub"), UpdatedAtMs: 1}

	ctx := context.Background()
	run := func(agent string, req *fleetv1.GitRequest) (*fleetv1.GitStatus, error) {
		req.Agent = agent
		r, err := m.gitAction(ctx, req)
		return r.GetStatus(), err
	}
	fetch := &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_FETCH}
	pull := &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_PULL}

	// Reading the checkout is no news from the agent: it stays read.
	if st, err := run("a1", &fleetv1.GitRequest{}); err != nil || st.Branch != "main" || st.Base != "" || !st.DefaultBranch {
		t.Fatalf("status: %v, %v", st, err)
	}
	if m.agents["a1"].UpdatedAtMs != 1 {
		t.Fatalf("a status read moved UpdatedAtMs to %d", m.agents["a1"].UpdatedAtMs)
	}

	// Not while the other agent in the checkout works.
	m.agents["a2"].State = stateWorking
	if _, err := run("a1", pull); err == nil || !strings.Contains(err.Error(), "two is working in the same checkout") {
		t.Fatalf("pull beside a working agent: %v", err)
	}
	m.agents["a2"].State = stateIdle

	// Uncommitted changes that conflict with the pull are no success.
	commitT(t, seed, "a.txt", "theirs\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("mine\n"), 0o644)
	if _, err := run("a1", pull); err == nil || !strings.Contains(err.Error(), "uncommitted changes conflict") {
		t.Fatalf("conflicting autostash: %v", err)
	}
	if st := m.agents["a1"].Git; st.GetConflicts() != 1 || gitT(t, work, "stash", "list") == "" {
		t.Fatalf("after the conflicting autostash: %v; stash %q", st, gitT(t, work, "stash", "list"))
	}
	gitT(t, work, "checkout", "-q", "--theirs", "a.txt")
	gitT(t, work, "reset", "-q")
	gitT(t, work, "checkout", "-q", "--", "a.txt")
	gitT(t, work, "stash", "drop", "-q")

	// main diverged from origin/main: it is never force pushed.
	commitT(t, work, "b.txt", "local\n")
	commitT(t, seed, "c.txt", "remote\n")
	gitT(t, seed, "push", "-q", "origin", "main")
	if st, err := run("a1", fetch); err != nil || st.Ahead != 1 || st.Behind != 1 {
		t.Fatalf("diverged: %v, %v", st, err)
	}
	if _, err := run("a1", &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_PUSH, Force: true}); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("force push to main: %v", err)
	}
	if _, err := run("a1", &fleetv1.GitRequest{Action: fleetv1.GitAction_GIT_ACTION_PUSH}); err == nil || !strings.Contains(err.Error(), "pull first") || strings.Contains(err.Error(), "force") {
		t.Fatalf("push to a diverged main: %v", err)
	}
	if got, want := gitT(t, origin, "rev-parse", "main"), gitT(t, seed, "rev-parse", "HEAD"); got != want {
		t.Fatal("origin/main was overwritten")
	}

	// A worktree's fetch shows as its own.
	wt := filepath.Join(dir, "wt")
	gitT(t, work, "worktree", "add", "-q", "-b", "fleet/wt", wt)
	m.agents["a3"] = &agentRec{ID: "a3", Name: "three", Adapter: "test", State: stateIdle, Cwd: wt}
	os.Remove(filepath.Join(work, ".git", "FETCH_HEAD"))
	if st, err := run("a3", fetch); err != nil || st.FetchedAtMs == 0 {
		t.Fatalf("worktree fetch: %v, %v", st, err)
	}
}
