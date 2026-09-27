package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
)

// fakeDocker is a docker CLI stand-in. The agent's `docker run -it` records
// its arguments in <dir>/run-args and sleeps like a running container;
// `rm` appends the container name to <dir>/rm; `ps` prints <dir>/ps.
const fakeDocker = `#!/bin/sh
D=%q
case "$1" in
version) echo 29.0 ;;
image) exit 0 ;;
ps) cat "$D/ps" 2>/dev/null; exit 0 ;;
rm) echo "$4" >> "$D/rm" ;;
run)
	for a in "$@"; do [ "$a" = "-it" ] && it=1; done
	if [ -n "$it" ]; then printf '%%s\n' "$@" > "$D/run-args"; exec sleep 60; fi
	exit 0 ;;
esac
`

// newSandboxEnv is a test daemon whose [sandbox] uses fakeDocker. It
// returns the directory fakeDocker records into.
func newSandboxEnv(t *testing.T) (*env, string) {
	e := newEnv(t)
	e.stop()
	rec := t.TempDir()
	bin := filepath.Join(rec, "docker")
	if err := os.WriteFile(bin, []byte(fmt.Sprintf(fakeDocker, rec)), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(e.home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[sandbox]\ndocker = %q\ndir = %q\nenv = [\"FLEET_TEST_PASS\"]\n", bin, filepath.Join(e.home, "sb"))
	f.Close()
	e.start()
	return e, rec
}

// runArgs waits for the agent's docker run and returns its arguments.
func runArgs(t *testing.T, rec string) []string {
	t.Helper()
	var b []byte
	waitFor(t, "docker run", func() bool {
		var err error
		b, err = os.ReadFile(filepath.Join(rec, "run-args"))
		return err == nil
	})
	os.Remove(filepath.Join(rec, "run-args"))
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// mounts returns the --mount values of a docker run command line.
func mounts(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestDockerSandbox(t *testing.T) {
	t.Setenv("FLEET_TEST_PASS", "secret")
	e, rec := newSandboxEnv(t)
	c := e.dialUnix()
	dir := e.mkdir("proj")
	sb := filepath.Join(e.home, "sb")

	a := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "proj", Sandbox: fleetv1.Sandbox_SANDBOX_DOCKER})
	if a.GetSandbox() != fleetv1.Sandbox_SANDBOX_DOCKER || a.GetIsolation() != isoPinned || a.GetCwd() != dir {
		t.Fatalf("agent = %v", a)
	}
	args := runArgs(t, rec)
	container := containerName(config.Config{TmuxSocket: e.tm.Socket}, a.GetId())
	state := filepath.Join(sb, "agents", a.GetId())
	matches, _ := filepath.Glob(filepath.Join(sb, "bin", "fleet-*"))
	if len(matches) != 1 {
		t.Fatalf("fleet binary copies: %v", matches)
	}
	fleetCopy := matches[0]
	if flagValue(args, "--name") != container || flagValue(args, "--workdir") != dir ||
		flagValue(args, "--env-file") != filepath.Join(state, envFile) ||
		!slices.Contains(args, "fleet.home="+config.Path()) || !slices.Contains(args, config.DefaultSandboxImage) {
		t.Errorf("docker run args: %q", args)
	}
	wantMounts := []string{
		"type=bind,source=" + dir + ",target=" + dir,
		"type=bind,source=" + state + ",target=" + state + ",readonly",
		"type=bind,source=" + filepath.Join(sb, "home") + ",target=" + sandboxHome,
		"type=bind,source=" + fleetCopy + ",target=" + fleetCopy + ",readonly",
	}
	if got := mounts(args); !slices.Equal(got, wantMounts) {
		t.Errorf("mounts =\n%q\nwant\n%q", got, wantMounts)
	}
	envb, _ := os.ReadFile(filepath.Join(state, envFile))
	for _, kv := range []string{"FLEET_HOME=" + state, "FLEET_AGENT_ID=" + a.GetId(), "HOME=" + sandboxHome,
		"FLEET_TEST_PASS=secret", "TEST_TRUST_DIR=" + dir} {
		if !strings.Contains(string(envb), kv+"\n") {
			t.Errorf("env file lacks %s:\n%s", kv, envb)
		}
	}

	// The hook socket delivers hooks for this agent only, whatever id the
	// sender claims, and nothing else.
	nc, err := net.Dial("unix", filepath.Join(state, hookSocket))
	if err != nil {
		t.Fatal(err)
	}
	hc := newClient(t, nc)
	hc.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
		AgentId: "someone-else", Event: "working", Payload: []byte("busy"),
	}}})
	c.waitAgent(a.GetId(), "working", func(a *fleetv1.Agent) bool {
		return a.GetState() == fleetv1.AgentState_AGENT_STATE_WORKING && a.GetStateDetail() == "busy"
	})
	hc.fails(listReq(true), codeDenied)

	// Killing removes the container and closes the hook socket.
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId()}))
	if b, _ := os.ReadFile(filepath.Join(rec, "rm")); !strings.Contains(string(b), container+"\n") {
		t.Errorf("container not removed; rm log: %q", b)
	}
	if _, err := os.Stat(filepath.Join(state, hookSocket)); !os.IsNotExist(err) {
		t.Errorf("hook socket still there: %v", err)
	}

	// At startup, containers of this home without a live agent are removed.
	os.WriteFile(filepath.Join(rec, "ps"), []byte("orphan-1\n"), 0o600)
	e.stop()
	e.start()
	waitFor(t, "orphan removal", func() bool {
		b, _ := os.ReadFile(filepath.Join(rec, "rm"))
		return strings.Contains(string(b), "orphan-1\n")
	})
}

func TestDockerSandboxWorktreeMounts(t *testing.T) {
	e, rec := newSandboxEnv(t)
	c := e.dialUnix()
	repo := e.mkdir("repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.RemoveAll(filepath.Join(repo, ".git", "hooks"))

	a := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "repo", Sandbox: fleetv1.Sandbox_SANDBOX_DOCKER})
	wt := a.GetCwd()
	if a.GetIsolation() != isoWorktree || !strings.HasPrefix(wt, filepath.Join(e.home, "sb", "worktrees")+"/") {
		t.Fatalf("agent = %v", a)
	}
	got := mounts(runArgs(t, rec))
	git := filepath.Join(repo, ".git")
	for _, m := range []string{
		"type=bind,source=" + wt + ",target=" + wt,
		"type=bind,source=" + git + ",target=" + git,
		"type=bind,source=" + filepath.Join(wt, ".git") + ",target=" + filepath.Join(wt, ".git") + ",readonly",
		"type=bind,source=" + filepath.Join(git, "config") + ",target=" + filepath.Join(git, "config") + ",readonly",
		"type=bind,source=" + filepath.Join(git, "hooks") + ",target=" + filepath.Join(git, "hooks") + ",readonly",
	} {
		if !slices.Contains(got, m) {
			t.Errorf("missing mount %s in\n%q", m, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(git, "hooks")); err != nil || !fi.IsDir() {
		t.Error("hooks dir was not created for its read-only mount")
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId(), RemoveWorktree: true}))
}

func TestSandboxAndCloneValidation(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	e.mkdir("p")
	invalid := codeInvalid
	c.fails(runReq(&fleetv1.RunAgentRequest{CloneUrl: "owner/repo", Root: "code"}), invalid)
	c.fails(runReq(&fleetv1.RunAgentRequest{CloneUrl: "owner/repo", Isolation: isoPinned}), invalid)
	c.fails(runReq(&fleetv1.RunAgentRequest{CloneUrl: "file:///etc"}), invalid)
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "code", Path: "p", Isolation: isoClone}), invalid)
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "code", Path: "p", Sandbox: 9}), invalid)

	// Without a usable docker, a sandboxed run fails up front.
	e.stop()
	f, _ := os.OpenFile(filepath.Join(e.home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, "\n[sandbox]\ndocker = %q\n", filepath.Join(t.TempDir(), "no-docker"))
	f.Close()
	e.start()
	c = e.dialUnix()
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "code", Path: "p", Sandbox: fleetv1.Sandbox_SANDBOX_DOCKER}),
		codeUnavailable)
}
