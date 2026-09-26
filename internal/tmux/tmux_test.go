package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// newTest returns a Tmux on a unique socket, killed on cleanup.
func newTest(t *testing.T) *Tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("FLEET_HOME", t.TempDir())
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	tm := New("fleet-test-" + hex.EncodeToString(b))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", tm.Socket, "kill-server").Run()
		// tmux leaves its socket file behind; remove ours.
		dir := os.Getenv("TMUX_TMPDIR")
		if dir == "" {
			dir = "/tmp"
		}
		_ = os.Remove(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), tm.Socket))
	})
	return tm
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitFor polls cond until true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func find(t *testing.T, tm *Tmux, name string) (PaneStatus, bool) {
	t.Helper()
	list, err := tm.List(ctxT(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range list {
		if p.Session == name {
			return p, true
		}
	}
	return PaneStatus{}, false
}

// assertNoSessions is the leak check: nothing may remain on the socket.
func assertNoSessions(t *testing.T, tm *Tmux) {
	t.Helper()
	list, err := tm.List(ctxT(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("leaked sessions: %+v", list)
	}
	out, err := exec.Command("tmux", "-L", tm.Socket, "list-sessions").CombinedOutput()
	if err == nil {
		t.Fatalf("server still has sessions: %s", out)
	}
}

func serverRunning(tm *Tmux) bool {
	return exec.Command("tmux", "-L", tm.Socket, "list-sessions").Run() == nil
}

func TestAvailable(t *testing.T) {
	tm := newTest(t)
	v, err := tm.Available(ctxT(t))
	if err != nil || !strings.HasPrefix(v, "tmux ") {
		t.Fatalf("Available = %q, %v", v, err)
	}
}

func TestListNoServer(t *testing.T) {
	tm := newTest(t)
	list, err := tm.List(ctxT(t))
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if err := tm.KillServer(ctxT(t)); err != nil {
		t.Fatalf("KillServer with no server: %v", err)
	}
	if err := tm.KillSession(ctxT(t), "fleet-nope"); err != nil {
		t.Fatalf("KillSession with no server: %v", err)
	}
}

func TestCreateListKill(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("a1")
	err := tm.NewSession(ctx, NewSessionOptions{
		Name: name, Cwd: t.TempDir(), Argv: []string{"sleep", "60"},
		Cols: 100, Rows: 30,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// A non-fleet session on the same socket is filtered out of List.
	if out, err := exec.Command("tmux", "-L", tm.Socket, "new-session", "-d", "-s", "other", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("other session: %v %s", err, out)
	}
	p, ok := find(t, tm, name)
	if !ok {
		t.Fatalf("session %s not listed", name)
	}
	if p.Dead || p.PanePID <= 0 || p.Attached != 0 {
		t.Fatalf("unexpected status %+v", p)
	}
	list, _ := tm.List(ctx)
	if len(list) != 1 {
		t.Fatalf("List should only contain fleet sessions: %+v", list)
	}
	out, err := exec.Command("tmux", "-L", tm.Socket, "display-message", "-p", "-t", "="+name+":",
		"#{window_width}x#{window_height} #{history_limit} #{remain-on-exit}").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "100x30 50000 on" {
		t.Fatalf("session options = %q", got)
	}
	if err := tm.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if _, ok := find(t, tm, name); ok {
		t.Fatal("session still listed after kill")
	}
	if err := tm.KillSession(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	assertNoSessions(t, tm)
}

func TestExitStatusCaptured(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("exit3")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"sh", "-c", "exit 3"}}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	var p PaneStatus
	waitFor(t, "dead pane", func() bool {
		var ok bool
		p, ok = find(t, tm, name)
		return ok && p.Dead
	})
	if p.ExitStatus != 3 {
		t.Fatalf("ExitStatus = %d, want 3", p.ExitStatus)
	}
	if err := tm.KillSession(ctx, name); err != nil {
		t.Fatal(err)
	}
	assertNoSessions(t, tm)
}

func TestSignalDeath(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("sig")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"sh", "-c", "kill -9 $$"}}); err != nil {
		t.Fatal(err)
	}
	var p PaneStatus
	waitFor(t, "dead pane", func() bool {
		var ok bool
		p, ok = find(t, tm, name)
		return ok && p.Dead
	})
	if p.Signal != 9 || p.ExitStatus != 137 || !p.Failed() || p.Detail() != "killed by signal 9" {
		t.Fatalf("got %+v (detail %q)", p, p.Detail())
	}
}

func TestExtraWindowIgnored(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("win")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatal(err)
	}
	// A user opens a window in the agent's session, and it exits non-zero.
	if _, err := tm.run(ctx, "new-window", "-t", exact(name)+":", "sh", "-c", "exit 3"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	p, ok := find(t, tm, name)
	if !ok || p.Dead {
		t.Fatalf("agent pane reported as %+v (found %v), want alive", p, ok)
	}
}

func TestSemicolonArgs(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	dir := filepath.Join(t.TempDir(), "dir;")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	name := SessionName("semi")
	err := tm.NewSession(ctx, NewSessionOptions{
		Name: name,
		Cwd:  dir,
		Env:  map[string]string{"V": "v;"},
		// If "a;" split the command, "new-session -d -s fleet-injected" would run.
		Argv: []string{"sh", "-c", `printf '%s|%s|%s|%s' "$PWD" "$V" "$1" "$2" > "$0"; exec sleep 30`, out, "a;", `b\;`, ";", "new-session", "-d", "-s", "fleet-injected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := dir + "|v;|a;|" + `b\;`
	waitFor(t, "output", func() bool {
		b, _ := os.ReadFile(out)
		return string(b) == want
	})
	if _, ok := find(t, tm, SessionName("injected")); ok {
		t.Fatal("argument ending in ';' was run as a tmux command")
	}
	if err := tm.SendText(ctx, name, "echo hi;", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "typed text", func() bool {
		b, _ := exec.Command("tmux", "-L", tm.Socket, "capture-pane", "-p", "-t", "="+name+":").Output()
		return strings.Contains(string(b), "echo hi;")
	})
}

func TestEnvAndSingleWordArgv(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("env")
	t.Setenv("FLEET_TEST_UNSET", "leak")
	// Single-word argv must not go through a shell: "$HOME" stays literal
	// and is looked up as a program name, which fails (exit 127).
	lit := SessionName("lit")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: lit, Argv: []string{"echo $HOME"}}); err != nil {
		t.Fatal(err)
	}
	err := tm.NewSession(ctx, NewSessionOptions{
		Name:     name,
		Argv:     []string{"sh", "-c", `printf '[%s][%s]\n' "$FOO" "${FLEET_TEST_UNSET-unset}"; exit 0`},
		Env:      map[string]string{"FOO": "b a;r"},
		UnsetEnv: []string{"FLEET_TEST_UNSET"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "output", func() bool {
		// The dead-pane notice scrolls output into history; capture it all.
		out, _ := exec.Command("tmux", "-L", tm.Socket, "capture-pane", "-p", "-S", "-", "-t", "="+name+":").Output()
		return strings.Contains(string(out), "[b a;r][unset]")
	})
	waitFor(t, "literal argv dead", func() bool {
		p, ok := find(t, tm, lit)
		return ok && p.Dead && p.ExitStatus == 127
	})
	for _, n := range []string{name, lit} {
		if err := tm.KillSession(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	assertNoSessions(t, tm)
}

func TestKillSessionIdempotent(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	a, keep := SessionName("x"), SessionName("xy")
	for _, n := range []string{a, keep} {
		if err := tm.NewSession(ctx, NewSessionOptions{Name: n, Argv: []string{"sleep", "60"}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := tm.KillSession(ctx, a); err != nil {
			t.Fatalf("KillSession #%d: %v", i, err)
		}
	}
	// Exact matching: killing "fleet-x" must not have hit "fleet-xy",
	// and killing a prefix of an existing name is a no-op.
	if _, ok := find(t, tm, keep); !ok {
		t.Fatal("exact-match violated: fleet-xy was killed")
	}
	if err := tm.KillSession(ctx, "fleet-"); err != nil {
		t.Fatal(err)
	}
	if _, ok := find(t, tm, keep); !ok {
		t.Fatal("prefix kill hit fleet-xy")
	}
	if err := tm.KillSession(ctx, keep); err != nil {
		t.Fatal(err)
	}
	assertNoSessions(t, tm)
}

func TestSendText(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("cat")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"cat"}}); err != nil {
		t.Fatal(err)
	}
	text := "hello; -t Enter $HOME"
	if err := tm.SendText(ctx, name, text, true); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	// cat echoes the line back after Enter, so it appears twice.
	waitFor(t, "echoed text", func() bool {
		out, _ := exec.Command("tmux", "-L", tm.Socket, "capture-pane", "-p", "-t", "="+name+":").Output()
		return strings.Count(string(out), text) == 2
	})
	if err := tm.SendText(ctx, "fleet-missing", "x", false); err == nil {
		t.Fatal("SendText to missing session should fail")
	}
	if err := tm.KillSession(ctx, name); err != nil {
		t.Fatal(err)
	}
	assertNoSessions(t, tm)
}

func TestServerExitsWhenEmpty(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("last")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	if !serverRunning(tm) {
		t.Fatal("server not running after NewSession")
	}
	if err := tm.KillSession(ctx, name); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "server exit", func() bool { return !serverRunning(tm) })
	assertNoSessions(t, tm)
}

func TestKillServer(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	for _, id := range []string{"k1", "k2"} {
		if err := tm.NewSession(ctx, NewSessionOptions{Name: SessionName(id), Argv: []string{"sleep", "60"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tm.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tm.KillServer(ctx); err != nil {
		t.Fatalf("second KillServer: %v", err)
	}
	assertNoSessions(t, tm)
}

// syncBuf is a goroutine-safe buffer for pty output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAttachCommandInPTY(t *testing.T) {
	tm := newTest(t)
	ctx := ctxT(t)
	name := SessionName("att")
	if err := tm.NewSession(ctx, NewSessionOptions{Name: name, Argv: []string{"cat"}}); err != nil {
		t.Fatal(err)
	}
	cmd := tm.AttachCommand(name, true)
	if !strings.Contains(strings.Join(cmd.Args, " "), "attach-session -t ="+name+" -r") {
		t.Fatalf("unexpected args %q", cmd.Args)
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer f.Close()
	var out syncBuf
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, f); close(done) }()

	waitFor(t, "client attached", func() bool {
		p, ok := find(t, tm, name)
		return ok && p.Attached == 1
	})
	if err := tm.SendText(ctx, name, "marker-42", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "output through attached client", func() bool {
		return strings.Contains(out.String(), "marker-42")
	})
	if err := tm.KillSession(ctx, name); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	<-done
	assertNoSessions(t, tm)
}

func TestParseList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []PaneStatus
		err  bool
	}{
		{name: "empty", in: ""},
		{name: "alive", in: "fleet-a\t%0\t12\t0\t\t\t1\t1\n", want: []PaneStatus{{Session: "fleet-a", PanePID: 12, Attached: 1}}},
		{name: "dead", in: "fleet-b\t%1\t13\t1\t3\t\t0\t1\n", want: []PaneStatus{{Session: "fleet-b", PanePID: 13, Dead: true, ExitStatus: 3}}},
		{name: "signal", in: "fleet-b\t%1\t13\t1\t\t9\t0\t1\n", want: []PaneStatus{{Session: "fleet-b", PanePID: 13, Dead: true, ExitStatus: 137, Signal: 9}}},
		{
			name: "extra window dead, agent alive",
			in:   "fleet-c\t%2\t20\t0\t\t\t1\t1\nfleet-c\t%3\t21\t1\t3\t\t1\t\n",
			want: []PaneStatus{{Session: "fleet-c", PanePID: 20, Attached: 1}},
		},
		{
			name: "agent dead, extra window alive",
			in:   "fleet-c\t%3\t21\t0\t\t\t1\t\nfleet-c\t%2\t20\t1\t0\t\t1\t1\n",
			want: []PaneStatus{{Session: "fleet-c", PanePID: 20, Dead: true, Attached: 1}},
		},
		{
			name: "unmarked falls back to oldest pane",
			in:   "fleet-d\t%9\t31\t1\t2\t\t0\t\nfleet-d\t%4\t30\t0\t\t\t0\t\n",
			want: []PaneStatus{{Session: "fleet-d", PanePID: 30}},
		},
		{name: "filtered", in: "main\t%0\t1\t0\t\t\t0\t\n"},
		{name: "garbage", in: "x\ty\n", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseList(tc.in)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %+v want %+v", got[i], tc.want[i])
				}
			}
		})
	}
}
