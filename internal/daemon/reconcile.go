package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/tmux"
)

const (
	reconcileInterval = time.Second
	// quickFailure: a non-zero exit this soon after start means the agent
	// could not start; its last output becomes the state detail. Deaths are
	// observed once per reconcileInterval, which is added as tolerance.
	quickFailure = 2 * time.Second
	// failureLines is how much pane output is kept for a quick failure.
	failureLines    = 20
	maxFailureBytes = 2000
)

// loop reconciles the registry with tmux until ctx is cancelled.
func (m *manager) loop(ctx context.Context) {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// adopt runs once at startup: live agents whose session still exists are
// re-adopted, the rest are marked EXITED, and fleet-* sessions without a
// live agent (orphans) are killed.
func (m *manager) adopt(ctx context.Context) error {
	panes, err := m.d.tmux.List(ctx)
	if err != nil {
		return fmt.Errorf("list tmux sessions: %w", err)
	}
	sessions := map[string]bool{}
	for _, p := range panes {
		sessions[p.Session] = true
	}
	m.mu.Lock()
	owned := map[string]bool{}
	adopted := 0
	for _, a := range m.sortedLocked() {
		if !a.live() {
			continue
		}
		if !sessions[a.TmuxSession] {
			a.finish(stateExited, "session disappeared while the daemon was stopped", nil)
			m.changedLocked(a)
			continue
		}
		owned[a.TmuxSession] = true
		adopted++
		if a.State == stateStarting {
			a.State = stateRunning
			m.changedLocked(a)
		}
	}
	m.trimHistoryLocked()
	m.saveLocked()
	m.mu.Unlock()

	for _, p := range panes {
		if owned[p.Session] {
			continue
		}
		m.d.log.Warn("killing orphan tmux session", "session", p.Session)
		if err := m.d.tmux.KillSession(ctx, p.Session); err != nil {
			m.d.log.Warn("killing orphan failed", "session", p.Session, "err", err)
		}
	}
	if adopted > 0 {
		m.d.log.Info("re-adopted running agents", "count", adopted)
	}
	return nil
}

// reconcile syncs agent states with tmux: dead panes are recorded and their
// sessions killed, vanished sessions mark the agent EXITED, and attached
// client counts are refreshed.
func (m *manager) reconcile(ctx context.Context) {
	// Agents that become ready after this point may be missing from the
	// snapshot below; they are left for the next round.
	m.mu.Lock()
	snap := m.readyGen
	m.mu.Unlock()
	panes, err := m.d.tmux.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.d.log.Warn("tmux list failed", "err", err)
		}
		return
	}
	bySession := make(map[string]tmux.PaneStatus, len(panes))
	for _, p := range panes {
		bySession[p.Session] = p
	}
	type deadPane struct {
		a     *agentRec
		p     tmux.PaneStatus
		quick bool
	}
	var dead []deadPane

	m.mu.Lock()
	now := time.Now()
	finished := false
	for _, a := range m.agents {
		if !a.live() || a.busy || a.readyGen > snap {
			continue
		}
		p, ok := bySession[a.TmuxSession]
		switch {
		case !ok:
			a.finish(stateExited, "session disappeared", nil)
			m.changedLocked(a)
			finished = true
		case p.Dead:
			quick := p.Failed() && now.Sub(time.UnixMilli(a.StartedAtMs)) < quickFailure+reconcileInterval
			dead = append(dead, deadPane{a, p, quick})
			a.busy = true
		case int32(p.Attached) != a.attached:
			a.attached = int32(p.Attached)
			m.changedLocked(a)
		}
	}
	m.mu.Unlock()

	for _, x := range dead {
		state, detail := stateExited, x.p.Detail()
		if x.quick {
			state = stateFailed
			if out := m.capture(ctx, x.p.Session); out != "" {
				detail = out
			}
		}
		if err := m.d.tmux.KillSession(ctx, x.p.Session); err != nil {
			m.d.log.Warn("killing dead session failed", "session", x.p.Session, "err", err)
		}
		m.mu.Lock()
		m.readyLocked(x.a)
		if x.a.live() {
			exit := x.p.ExitStatus
			x.a.finish(state, detail, &exit)
			m.changedLocked(x.a)
			finished = true
		}
		m.mu.Unlock()
		m.d.log.Info("agent exited", "agent", x.a.ID, "status", x.p.ExitStatus, "signal", x.p.Signal, "state", stateName(state))
	}
	if finished {
		m.mu.Lock()
		m.trimHistoryLocked()
		m.mu.Unlock()
	}
}

func stateName(s fleetv1.AgentState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "AGENT_STATE_"))
}

// capture returns the last lines of a (dead) pane's output. tmux prints a
// "Pane is dead" status line at the bottom, which is dropped.
func (m *manager) capture(ctx context.Context, session string) string {
	bin := m.d.tmux.Binary
	if bin == "" {
		bin = "tmux"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-L", m.d.tmux.Socket,
		"capture-pane", "-p", "-J", "-t", "="+session+":", "-S", fmt.Sprint(-failureLines*3))
	cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(bytes.TrimRight(out, "\n ")), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.HasPrefix(l, "Pane is dead") {
			continue
		}
		lines = append(lines, l)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) > failureLines {
		lines = lines[len(lines)-failureLines:]
	}
	s := strings.Join(lines, "\n")
	if len(s) > maxFailureBytes {
		s = strings.ToValidUTF8(s[len(s)-maxFailureBytes:], "")
	}
	return s
}

// envWithout returns env minus the named variables.
func envWithout(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
outer:
	for _, kv := range env {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}
