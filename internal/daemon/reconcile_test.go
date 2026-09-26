package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fleet/internal/tmux"
)

// An agent that finishes starting while reconcile's tmux snapshot is being
// taken must not be marked EXITED because the snapshot lacks its session.
func TestReconcileSkipsAgentsReadyAfterSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	// A fake tmux whose list-panes is slow and lists no sessions.
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), tmux: &tmux.Tmux{Socket: "fleet-test-unused", Binary: bin}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	a := &agentRec{ID: "a1", State: stateStarting, TmuxSession: tmux.SessionName("a1"), busy: true}
	m.agents[a.ID] = a

	done := make(chan struct{})
	go func() { m.reconcile(context.Background()); close(done) }()
	time.Sleep(100 * time.Millisecond) // reconcile is inside List now
	m.mu.Lock()
	m.readyLocked(a) // run() finished NewSession after the snapshot began
	a.State, a.StartedAtMs = stateRunning, time.Now().UnixMilli()
	m.mu.Unlock()
	<-done

	m.mu.Lock()
	defer m.mu.Unlock()
	if a.State != stateRunning {
		t.Fatalf("state %v (%s), want RUNNING", a.State, a.StateDetail)
	}
}

// An agent killed by a signal right after start is a quick failure with
// exit code 128+signal, not a clean exit with status 0.
func TestReconcileSignalDeathIsFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	bin := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\ncase \"$*\" in *list-panes*) printf 'fleet-a1\\t%%0\\t42\\t1\\t\\t9\\t0\\t1\\n';; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), tmux: &tmux.Tmux{Socket: "fleet-test-unused", Binary: bin}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	a := &agentRec{ID: "a1", State: stateRunning, TmuxSession: tmux.SessionName("a1"), StartedAtMs: time.Now().UnixMilli()}
	m.agents[a.ID] = a
	m.reconcile(context.Background())
	if a.State != stateFailed || !a.HasExitCode || a.ExitCode != 137 || a.StateDetail != "killed by signal 9" {
		t.Fatalf("got state %v code %v/%d detail %q", a.State, a.HasExitCode, a.ExitCode, a.StateDetail)
	}
}
