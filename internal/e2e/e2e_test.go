// Package e2e drives a real fleet binary end to end: daemon, CLI, tmux,
// git worktrees, TLS pairing and daemon restarts.
//
// Every tmux call uses a unique "fleet-test-<random>" socket that is
// killed in cleanup; the user's default tmux server and the "fleet"
// socket are never touched.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"fleet/internal/client"
)

// env is one test run: the built binary, the daemon's home and a client home.
type env struct {
	t          *testing.T
	bin        string
	home       string // daemon machine FLEET_HOME
	clientHome string // second machine FLEET_HOME
	sock       string // tmux -L socket
	port       int
	root       string // allowed root
	repo       string // git repo inside root
	daemon     *exec.Cmd
	daemonDone chan error
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped with -short")
	}
	for _, tool := range []string{"tmux", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("e2e: %s not installed", tool)
		}
	}
	e := setup(t)

	e.startDaemon()
	e.mustFleet(e.home, "roots", "add", e.root, "--name", "code")
	if out := e.mustFleet(e.home, "adapters"); !strings.Contains(out, "shell") {
		t.Fatalf("adapters does not list shell:\n%s", out)
	}

	t.Run("pinned shell", e.pinnedShell)
	t.Run("worktree", e.worktree)
	t.Run("remote pairing", e.remote)
	t.Run("restart", e.restart)

	e.stopDaemon()
}

func setup(t *testing.T) *env {
	t.Helper()
	tmp := t.TempDir()
	e := &env{
		t:          t,
		bin:        filepath.Join(tmp, "fleet"),
		home:       filepath.Join(tmp, "server-home"),
		clientHome: filepath.Join(tmp, "client-home"),
		sock:       "fleet-test-" + randHex(6),
		port:       freePort(t),
		root:       filepath.Join(tmp, "code"),
	}
	// Resolve symlinks (e.g. /tmp on macOS) so paths compare equal to the daemon's.
	must(t, os.MkdirAll(e.root, 0o755))
	root, err := filepath.EvalSymlinks(e.root)
	must(t, err)
	e.root = root
	e.repo = filepath.Join(e.root, "repo")

	t.Cleanup(func() {
		if e.daemon != nil {
			e.stopDaemon()
		}
		_ = exec.Command("tmux", "-L", e.sock, "kill-server").Run()
		if dir := tmuxSocketDir(); dir != "" {
			_ = os.Remove(filepath.Join(dir, e.sock))
		}
	})

	build := exec.Command("go", "build", "-o", e.bin, "fleet/cmd/fleet")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	for _, h := range []string{e.home, e.clientHome} {
		must(t, os.MkdirAll(h, 0o700))
	}
	cfg := fmt.Sprintf("name = \"e2e-server\"\nlisten = \"127.0.0.1:%d\"\nmdns = false\ntmux_socket = %q\n", e.port, e.sock)
	must(t, os.WriteFile(filepath.Join(e.home, "config.toml"), []byte(cfg), 0o600))

	must(t, os.MkdirAll(e.repo, 0o755))
	e.git("init", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(e.repo, "README"), []byte("e2e\n"), 0o644))
	e.git("add", "README")
	e.git("commit", "-q", "-m", "init")
	return e
}

// pinnedShell runs a shell in the repo, types into it and reads it back
// through a Go client attach, then kills it and checks tmux is clean.
func (e *env) pinnedShell(t *testing.T) {
	e.mustFleet(e.home, "run", "shell", e.repo, "--pinned", "--name", "sh1")
	a := e.waitAgent("sh1", func(a agent) bool { return a.State == "AGENT_STATE_RUNNING" })
	if a.Cwd != e.repo || a.Branch != "" {
		t.Fatalf("pinned agent cwd=%q branch=%q", a.Cwd, a.Branch)
	}
	if out := e.mustFleet(e.home, "ls"); !strings.Contains(out, "sh1") || !strings.Contains(out, "running") {
		t.Fatalf("ls does not show sh1 running:\n%s", out)
	}

	e.mustFleet(e.home, "send", "sh1", "echo e2e-marker")

	t.Setenv("FLEET_HOME", e.home)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := client.DialLocal(ctx)
	must(t, err)
	defer c.Close()
	term, err := c.Attach(ctx, "sh1", client.AttachOptions{Cols: 100, Rows: 30, ReadOnly: true})
	must(t, err)
	var screen bytes.Buffer
	// "e2e-marker" appears once in the typed command; wait for the echoed output too.
	for strings.Count(screen.String(), "e2e-marker") < 2 {
		select {
		case b, ok := <-term.Output():
			if !ok {
				t.Fatalf("terminal closed (%s) before marker; got %q", term.Reason(), screen.String())
			}
			screen.Write(b)
		case <-ctx.Done():
			t.Fatalf("no e2e-marker output in attach; got %q", screen.String())
		}
	}
	must(t, term.Detach(ctx))

	e.mustFleet(e.home, "kill", "sh1")
	e.waitNoSessions()
	e.waitTmuxServerGone()
}

// worktree runs a shell in its own worktree and removes it on kill.
func (e *env) worktree(t *testing.T) {
	e.mustFleet(e.home, "run", "shell", e.repo, "--worktree", "--name", "wt1")
	a := e.waitAgent("wt1", func(a agent) bool { return a.State == "AGENT_STATE_RUNNING" })
	if a.Branch != "fleet/wt1" || a.Cwd == e.repo {
		t.Fatalf("worktree agent cwd=%q branch=%q", a.Cwd, a.Branch)
	}
	if fi, err := os.Stat(a.Cwd); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir %s missing: %v", a.Cwd, err)
	}
	if list := e.git("worktree", "list"); !strings.Contains(list, a.Cwd) || !strings.Contains(list, "[fleet/wt1]") {
		t.Fatalf("git worktree list:\n%s", list)
	}
	e.git("rev-parse", "--verify", "refs/heads/fleet/wt1")

	e.mustFleet(e.home, "kill", "wt1", "--rm-worktree")
	if _, err := os.Stat(a.Cwd); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir still exists: %v", err)
	}
	if list := e.git("worktree", "list"); strings.Contains(list, a.Cwd) {
		t.Fatalf("worktree still registered:\n%s", list)
	}
	e.waitNoSessions()
}

// remote pairs a second FLEET_HOME over TLS, uses it, then revokes it.
func (e *env) remote(t *testing.T) {
	out := e.mustFleet(e.home, "pair")
	code, fp := "", ""
	fields := strings.Fields(out)
	for i, f := range fields {
		if len(f) == 9 && f[4] == '-' {
			code = f
		}
		if f == "--fingerprint" && i+1 < len(fields) {
			fp = fields[i+1]
		}
	}
	if code == "" || fp == "" {
		t.Fatalf("no pairing code or fingerprint in:\n%s", out)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", e.port)
	// A wrong fingerprint stops pairing before the code is sent.
	if out, err := e.fleet(e.clientHome, "connect", addr, "--code", code, "--fingerprint", strings.Repeat("0", 16)); err == nil ||
		!strings.Contains(out, "fingerprint") {
		t.Fatalf("connect with wrong fingerprint: %v\n%s", err, out)
	}
	e.mustFleet(e.clientHome, "connect", addr, "--code", code, "--fingerprint", fp, "--name", "e2e-client")
	if out := e.mustFleet(e.clientHome, "-H", "e2e-server", "ls", "-a"); !strings.Contains(out, "sh1") {
		t.Fatalf("remote ls:\n%s", out)
	}
	if out := e.mustFleet(e.home, "devices"); !strings.Contains(out, "e2e-client") {
		t.Fatalf("devices:\n%s", out)
	}

	e.mustFleet(e.home, "devices", "revoke", "e2e-client")
	out, err := e.fleet(e.clientHome, "-H", "e2e-server", "ls")
	if err == nil {
		t.Fatalf("remote ls after revoke succeeded:\n%s", out)
	}
	if !strings.Contains(out, "revoked") && !strings.Contains(strings.ToLower(out), "auth") {
		t.Fatalf("remote ls after revoke: want an auth error, got:\n%s", out)
	}
}

// restart checks agents survive a daemon restart and orphans are killed.
func (e *env) restart(t *testing.T) {
	e.mustFleet(e.home, "run", "shell", e.repo, "--pinned", "--name", "long")
	a := e.waitAgent("long", func(a agent) bool { return a.State == "AGENT_STATE_RUNNING" })

	e.stopDaemon()
	if !e.hasSession(a.TmuxSession) {
		t.Fatalf("session %s died with the daemon", a.TmuxSession)
	}
	e.tmux("new-session", "-d", "-s", "fleet-zzzzzz")

	e.startDaemon()
	b := e.waitAgent("long", func(a agent) bool { return a.State == "AGENT_STATE_RUNNING" })
	if b.ID != a.ID {
		t.Fatalf("re-adopted agent id %s, want %s", b.ID, a.ID)
	}
	if e.hasSession("fleet-zzzzzz") {
		t.Fatal("orphan fleet-zzzzzz session survived the restart")
	}
	if !e.hasSession(a.TmuxSession) {
		t.Fatal("re-adopted session is gone")
	}
	e.mustFleet(e.home, "kill", "long")
	e.waitNoSessions()
}

// --- daemon lifecycle ---

func (e *env) startDaemon() {
	e.t.Helper()
	logf, err := os.OpenFile(filepath.Join(e.home, "e2e-daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	must(e.t, err)
	cmd := exec.Command(e.bin, "daemon")
	cmd.Env = e.environ(e.home)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	must(e.t, cmd.Start())
	logf.Close()
	e.daemon, e.daemonDone = cmd, make(chan error, 1)
	go func(done chan error) { done <- cmd.Wait() }(e.daemonDone)

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := e.fleet(e.home, "status"); err == nil {
			return
		}
		select {
		case err := <-e.daemonDone:
			e.daemon = nil
			e.t.Fatalf("daemon exited during startup: %v\n%s", err, e.daemonLog())
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("daemon did not come up\n%s", e.daemonLog())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *env) stopDaemon() {
	e.t.Helper()
	if e.daemon == nil {
		return
	}
	_ = e.daemon.Process.Signal(syscall.SIGTERM)
	select {
	case <-e.daemonDone:
	case <-time.After(15 * time.Second):
		_ = e.daemon.Process.Kill()
		<-e.daemonDone
		e.t.Errorf("daemon ignored SIGTERM\n%s", e.daemonLog())
	}
	e.daemon = nil
}

func (e *env) daemonLog() string {
	b, _ := os.ReadFile(filepath.Join(e.home, "e2e-daemon.log"))
	return string(b)
}

// --- CLI ---

// environ is the process environment with FLEET_HOME set to home and
// anything that could redirect fleet or tmux elsewhere removed.
func (e *env) environ(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "FLEET_HOME", "FLEET_HOST", "FLEET_AGENT_ID", "FLEET_SOCKET", "TMUX", "TMUX_PANE":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "FLEET_HOME="+home, "SHELL=/bin/sh")
}

func (e *env) fleet(home string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = e.environ(home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustFleet(home string, args ...string) string {
	e.t.Helper()
	out, err := e.fleet(home, args...)
	if err != nil {
		e.t.Fatalf("fleet %s: %v\n%s\ndaemon log:\n%s", strings.Join(args, " "), err, out, e.daemonLog())
	}
	return out
}

type agent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Cwd         string `json:"cwd"`
	Branch      string `json:"branch"`
	TmuxSession string `json:"tmux_session"`
}

func (e *env) agents() []agent {
	e.t.Helper()
	out := e.mustFleet(e.home, "ls", "--json")
	var list []agent
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		e.t.Fatalf("ls --json: %v\n%s", err, out)
	}
	return list
}

// waitAgent polls `fleet ls` until the named agent satisfies ok.
func (e *env) waitAgent(name string, ok func(agent) bool) agent {
	e.t.Helper()
	var last []agent
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		last = e.agents()
		for _, a := range last {
			if a.Name == name && ok(a) {
				return a
			}
		}
	}
	e.t.Fatalf("agent %s never reached the wanted state; ls: %+v", name, last)
	return agent{}
}

// --- tmux and git ---

func (e *env) tmux(args ...string) string {
	e.t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", e.sock}, args...)...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// sessions lists sessions on the test socket; nil if no server runs.
func (e *env) sessions() []string {
	out, err := exec.Command("tmux", "-L", e.sock, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func (e *env) hasSession(name string) bool {
	for _, s := range e.sessions() {
		if s == name {
			return true
		}
	}
	return false
}

func (e *env) waitNoSessions() {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		n := 0
		for _, s := range e.sessions() {
			if strings.HasPrefix(s, "fleet-") {
				n++
			}
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatalf("fleet-* sessions left: %v", e.sessions())
}

func (e *env) waitTmuxServerGone() {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if exec.Command("tmux", "-L", e.sock, "list-sessions").Run() != nil {
			return
		}
	}
	e.t.Fatalf("tmux server on %s still running: %v", e.sock, e.sessions())
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=e2e", "-c", "user.email=e2e@example.com"}, args...)...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// tmuxSocketDir is where tmux puts -L sockets.
func tmuxSocketDir() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()))
}

// --- helpers ---

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
