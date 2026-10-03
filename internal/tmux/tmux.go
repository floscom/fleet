// Package tmux drives a dedicated tmux server for fleet agents.
//
// All commands run with `tmux -L <socket>` so fleet never touches the user's
// own tmux server. Sessions are named "fleet-<agentID>". Sessions are created
// with remain-on-exit ON so the daemon can read the exit status of a dead
// pane — and the daemon is then responsible for killing the session (see
// Reap). The server option exit-empty is on, so the tmux server exits when
// the last fleet session is gone.
package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SessionPrefix prefixes every fleet session name.
const SessionPrefix = "fleet-"

// Tmux runs tmux commands against one socket.
type Tmux struct {
	// Socket is the -L name, e.g. "fleet". Tests use a unique name.
	Socket string
	// Binary is the tmux executable; "" means look up "tmux" in PATH.
	Binary string
}

// New returns a Tmux for socket.
func New(socket string) *Tmux { return &Tmux{Socket: socket} }

// SessionName returns SessionPrefix + agentID.
func SessionName(agentID string) string { return SessionPrefix + agentID }

// NewSessionOptions configures NewSession.
type NewSessionOptions struct {
	Name     string
	Cwd      string
	Argv     []string          // exec'd directly, no shell
	Env      map[string]string // set in the session environment (-e)
	UnsetEnv []string          // run via `env -u VAR ...` prefix
	Cols     int               // default 200
	Rows     int               // default 50
}

// Available reports whether tmux can be executed, with its version string.
func (t *Tmux) Available(ctx context.Context) (version string, err error) {
	out, err := exec.CommandContext(ctx, t.bin(), "-V").Output()
	if err != nil {
		return "", fmt.Errorf("tmux -V: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// NewSession creates a detached session running Argv, atomically setting
// remain-on-exit on and history-limit 50000 in the same tmux invocation (an
// agent can exit within milliseconds). Also sets server options exit-empty on
// and window-size latest.
func (t *Tmux) NewSession(ctx context.Context, o NewSessionOptions) error {
	if o.Name == "" {
		return errors.New("tmux: session name required")
	}
	if len(o.Argv) == 0 {
		return errors.New("tmux: argv required")
	}
	cols, rows := o.Cols, o.Rows
	if cols <= 0 {
		cols = 200
	}
	if rows <= 0 {
		rows = 50
	}
	// The socket is fleet-only, so global options are safe to set. Setting
	// remain-on-exit and history-limit globally BEFORE new-session closes the
	// race where an instantly exiting argv destroys the session (or the pane
	// is created with the default history) before per-session options apply.
	args := []string{
		"start-server", ";",
		"set-option", "-g", "remain-on-exit", "on", ";",
		"set-option", "-g", "history-limit", "50000", ";",
		"set-option", "-s", "exit-empty", "on", ";",
		"set-option", "-g", "window-size", "latest", ";",
		"new-session", "-d", "-s", o.Name,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows),
	}
	if o.Cwd != "" {
		args = append(args, "-c", escapeArg(o.Cwd))
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", escapeArg(k+"="+o.Env[k]))
	}
	// Always go through env(1): tmux hands a single-word command to
	// /bin/sh -c, and env guarantees argv is exec'd directly.
	args = append(args, "--", "env")
	for _, v := range o.UnsetEnv {
		args = append(args, "-u", escapeArg(v))
	}
	for _, v := range o.Argv {
		args = append(args, escapeArg(v))
	}
	// The new session has exactly one pane: the agent. It is marked with a
	// pane option so List can tell it apart from windows a user opened by
	// hand inside an attached session.
	args = append(args, ";",
		"set-option", "-w", "-t", exact(o.Name)+":", "remain-on-exit", "on", ";",
		"set-option", "-t", exact(o.Name)+":", "history-limit", "50000", ";",
		"set-option", "-p", "-t", exact(o.Name)+":", agentPaneOption, "1",
	)
	if _, err := t.run(ctx, args...); err != nil {
		return fmt.Errorf("new session %s: %w", o.Name, err)
	}
	return nil
}

// KillSession kills a session. Killing a session that no longer exists is not an error.
func (t *Tmux) KillSession(ctx context.Context, name string) error {
	_, err := t.run(ctx, "kill-session", "-t", exact(name))
	if err != nil && (isNoServer(err) || isNoSession(err)) {
		return nil
	}
	return err
}

// PaneStatus describes the agent pane of a fleet session.
type PaneStatus struct {
	Session string
	PanePID int
	Dead    bool
	// ExitStatus is valid when Dead. A pane killed by a signal reports
	// 128+Signal, like a shell.
	ExitStatus int
	// Signal is the signal that killed a dead pane, 0 if it exited normally.
	Signal   int
	Attached int // number of attached tmux clients
}

// Failed reports whether a dead pane exited non-zero or was killed by a signal.
func (p PaneStatus) Failed() bool { return p.Dead && (p.ExitStatus != 0 || p.Signal != 0) }

// Detail describes how a dead pane ended, e.g. "exited with status 1" or
// "killed by signal 9".
func (p PaneStatus) Detail() string {
	if p.Signal != 0 {
		return fmt.Sprintf("killed by signal %d", p.Signal)
	}
	return fmt.Sprintf("exited with status %d", p.ExitStatus)
}

// agentPaneOption marks the pane NewSession created for the agent.
const agentPaneOption = "@fleet_agent"

const listFormat = "#{session_name}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{session_attached}\t#{" + agentPaneOption + "}"

// List returns the status of the agent pane of every session on the socket
// whose name starts with SessionPrefix, one entry per session. Other panes
// (windows a user opened inside an attached session) are ignored; sessions
// created without the agent marker fall back to their oldest pane. No server
// running => empty list, nil error.
func (t *Tmux) List(ctx context.Context) ([]PaneStatus, error) {
	out, err := t.run(ctx, "list-panes", "-a", "-F", listFormat)
	if err != nil {
		if isNoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseList(out)
}

func parseList(out string) ([]PaneStatus, error) {
	type cand struct {
		ps     PaneStatus
		paneID int
		agent  bool
	}
	var order []string
	best := map[string]cand{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 8 {
			return nil, fmt.Errorf("tmux: unexpected list-panes line %q", line)
		}
		if !strings.HasPrefix(f[0], SessionPrefix) {
			continue
		}
		c := cand{ps: PaneStatus{Session: f[0], Dead: f[3] == "1"}, agent: f[7] == "1"}
		c.paneID, _ = strconv.Atoi(strings.TrimPrefix(f[1], "%"))
		c.ps.PanePID, _ = strconv.Atoi(f[2])
		c.ps.Signal, _ = strconv.Atoi(f[5])
		if st, err := strconv.Atoi(f[4]); err == nil {
			c.ps.ExitStatus = st
		} else if c.ps.Signal != 0 {
			c.ps.ExitStatus = 128 + c.ps.Signal
		}
		c.ps.Attached, _ = strconv.Atoi(f[6])
		old, seen := best[f[0]]
		switch {
		case !seen:
			order = append(order, f[0])
		case old.agent && !c.agent,
			old.agent == c.agent && old.paneID < c.paneID:
			continue
		}
		best[f[0]] = c
	}
	res := make([]PaneStatus, 0, len(order))
	for _, name := range order {
		res = append(res, best[name].ps)
	}
	return res, nil
}

// pasteOver is the text length from which SendText pastes instead of
// typing: tmux caps the size of a command, and so of send-keys arguments.
const pasteOver = 2048

// enterDelay is the pause between text and the Enter that submits it.
// Codex takes keys arriving in a burst for a paste, and an Enter inside
// one for a newline.
const enterDelay = 150 * time.Millisecond

// SendText types text into the session, then Enter if submit. Text with a
// newline, or long text, is pasted (see Paste) so that each newline does
// not act as Enter; the rest is typed literally (send-keys -l).
func (t *Tmux) SendText(ctx context.Context, session, text string, submit bool) error {
	target := exact(session) + ":"
	switch {
	case strings.Contains(text, "\n") || len(text) > pasteOver:
		if err := t.Paste(ctx, session, text); err != nil {
			return err
		}
	case text != "":
		if _, err := t.run(ctx, "send-keys", "-l", "-t", target, "--", escapeArg(text)); err != nil {
			return err
		}
	}
	if submit {
		if text != "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(enterDelay):
			}
		}
		if _, err := t.run(ctx, "send-keys", "-t", target, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

// Paste pastes text into the session through a temporary buffer, as a
// bracketed paste if the program in the pane asked for one (agent TUIs do):
// the program sees one paste, not typed lines. The text goes to tmux on
// stdin, so its size is not limited like a command argument.
func (t *Tmux) Paste(ctx context.Context, session, text string) error {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	buf := "fleet-paste-" + hex.EncodeToString(b[:])
	if _, err := t.runInput(ctx, strings.NewReader(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	// -d deletes the buffer after pasting; -p brackets the paste.
	if _, err := t.run(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", exact(session)+":"); err != nil {
		t.run(context.WithoutCancel(ctx), "delete-buffer", "-b", buf)
		return err
	}
	return nil
}

// SendKeys presses keys in the session, given as tmux key names such as
// Enter, Escape, Up or C-c.
func (t *Tmux) SendKeys(ctx context.Context, session string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := []string{"send-keys", "-t", exact(session) + ":"}
	for _, k := range keys {
		args = append(args, escapeArg(k))
	}
	_, err := t.run(ctx, args...)
	return err
}

// Screen returns the visible contents of the session's active pane, one line
// per screen row (wrapped lines joined).
func (t *Tmux) Screen(ctx context.Context, session string) (string, error) {
	return t.run(ctx, "capture-pane", "-p", "-J", "-t", exact(session)+":")
}

// AttachCommand returns an *exec.Cmd that attaches a new tmux client to the
// session (`tmux -L sock attach-session -t =name`, plus -r for read-only).
// The caller runs it inside a PTY.
func (t *Tmux) AttachCommand(session string, readOnly bool) *exec.Cmd {
	args := []string{"-L", t.Socket, "attach-session", "-t", exact(session)}
	if readOnly {
		args = append(args, "-r")
	}
	cmd := exec.Command(t.bin(), args...)
	cmd.Env = append(cleanEnv(), "TERM=xterm-256color")
	return cmd
}

// KillServer kills the whole fleet tmux server (used by tests and `fleet nuke`).
func (t *Tmux) KillServer(ctx context.Context) error {
	_, err := t.run(ctx, "kill-server")
	if err != nil && isNoServer(err) {
		return nil
	}
	return err
}

func (t *Tmux) bin() string {
	if t.Binary != "" {
		return t.Binary
	}
	return "tmux"
}

// run executes `tmux -L socket args...` and returns stdout. Errors carry
// tmux's stderr.
func (t *Tmux) run(ctx context.Context, args ...string) (string, error) {
	return t.runInput(ctx, nil, args...)
}

// runInput is run with stdin.
func (t *Tmux) runInput(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), append([]string{"-L", t.Socket}, args...)...)
	cmd.Env = cleanEnv()
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &Error{Args: args, Msg: msg}
	}
	return stdout.String(), nil
}

// Error is a failed tmux invocation.
type Error struct {
	Args []string
	Msg  string // tmux's stderr
}

func (e *Error) Error() string {
	return "tmux: " + e.Msg
}

func errMsg(err error) string {
	var te *Error
	if errors.As(err, &te) {
		return te.Msg
	}
	return ""
}

func isNoServer(err error) bool {
	m := errMsg(err)
	return strings.Contains(m, "no server running") ||
		strings.Contains(m, "error connecting to") ||
		strings.Contains(m, "server exited unexpectedly")
}

func isNoSession(err error) bool {
	m := errMsg(err)
	return strings.Contains(m, "can't find session") ||
		strings.Contains(m, "session not found")
}

// escapeArg protects a caller-supplied argument from tmux's command-line
// parser, which treats any argument ending in ';' as a command separator
// (even after "--"). A trailing ';' becomes `\;`, which tmux turns back
// into ';' — also for an argument that already ends in `\;`.
func escapeArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

// exact returns a tmux target that matches the session name exactly.
func exact(name string) string { return "=" + name }

// cleanEnv is os.Environ without TMUX/TMUX_PANE, so a daemon started inside
// someone's tmux never talks to (or nests warnings about) that server.
func cleanEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
