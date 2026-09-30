package daemon

// The dashboard's chat view: where an agent's transcript is, its screen,
// and keys typed into it.

import (
	"context"
	"path/filepath"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

// screenTimeout bounds capturing an agent's screen.
const screenTimeout = 3 * time.Second

// home is the host directory a sandboxed agent uses as its home (mounted at
// sandboxHome), "" for agents that use the daemon user's home.
func (a *agentRec) home() string {
	if a.Container == "" || a.StateDir == "" {
		return ""
	}
	// StateDir is <sandbox dir>/agents/<id>; the home is <sandbox dir>/home.
	return filepath.Join(filepath.Dir(filepath.Dir(a.StateDir)), "home")
}

// transcriptFile maps the transcript path a hook reported to this host and
// returns it if it lies in the adapter's transcript dir, else "". Hooks of
// sandboxed agents come from inside the container: their paths are
// container paths, and must not point the daemon at other files.
func transcriptFile(ad adapter.Adapter, a *agentRec, reported string) string {
	t, ok := ad.(adapter.Transcripter)
	if !ok || !filepath.IsAbs(reported) {
		return ""
	}
	p := filepath.Clean(reported)
	home := a.home()
	if home != "" {
		rel, err := filepath.Rel(sandboxHome, p)
		if err != nil || !filepath.IsLocal(rel) {
			return ""
		}
		p = filepath.Join(home, rel)
	}
	if !within(filepath.Clean(t.TranscriptDir(home)), p) {
		return ""
	}
	return p
}

// inTranscriptDir reports whether path, with symlinks resolved, is inside
// dir: a sandboxed agent could leave a symlink in its home that points
// anywhere on the host.
func inTranscriptDir(dir, path string) bool {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	return err == nil && within(realDir, real)
}

// chat returns an agent, its transcript file and the parser for it. The
// path is "" while there is no transcript (yet), or when the adapter keeps
// none (parse is then nil too).
func (m *manager) chat(ref string) (*fleetv1.Agent, string, transcript.Parser, error) {
	s, err := m.session(ref)
	if s.t == nil {
		return s.agent, "", nil, err
	}
	return s.agent, s.path, s.t.ParseTranscript, nil
}

// sessionFiles is where an agent's session is recorded.
type sessionFiles struct {
	agent *fleetv1.Agent
	// t is the agent's adapter if it keeps transcripts (else nil), dir its
	// transcript dir, path the transcript ("" while there is none).
	t    adapter.Transcripter
	dir  string
	path string
}

// session finds an agent's transcript.
func (m *manager) session(ref string) (sessionFiles, error) {
	m.mu.Lock()
	a, err := m.find(ref)
	if err != nil {
		m.mu.Unlock()
		return sessionFiles{}, err
	}
	pa, adapterID, sessionID, path, home := a.proto(), a.Adapter, a.SessionID, a.Transcript, a.home()
	m.mu.Unlock()

	ad, _ := m.d.opts.Adapters.Get(adapterID)
	t, ok := ad.(adapter.Transcripter)
	if !ok {
		return sessionFiles{agent: pa}, nil
	}
	dir := t.TranscriptDir(home)
	if path == "" && sessionID != "" {
		// No hook reported it (older agents, or none has run yet): the
		// session id finds it.
		if p, found := t.FindTranscript(dir, sessionID); found {
			path = p
			m.mu.Lock()
			if a.Transcript == "" && a.SessionID == sessionID {
				a.Transcript = p
				m.saveLocked()
			}
			m.mu.Unlock()
		}
	}
	if path != "" && !inTranscriptDir(dir, path) {
		path = ""
	}
	return sessionFiles{agent: pa, t: t, dir: dir, path: path}, nil
}

// liveSession returns the tmux session and adapter of a live agent.
func (m *manager) liveSession(ref string) (session, adapterID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.find(ref)
	if err != nil {
		return "", "", err
	}
	if !a.live() {
		return "", "", errf(codeInvalid, "agent %s is not running", a.ID)
	}
	return a.TmuxSession, a.Adapter, nil
}

// screen returns the visible screen of a live agent.
func (m *manager) screen(ctx context.Context, ref string) (string, error) {
	session, _, err := m.liveSession(ref)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()
	return m.d.tmux.Screen(ctx, session)
}

// sendInput types text into a live agent (then Enter if submit) and presses
// keys (tmux key names). With guard, Enter is not pressed after text while
// the agent shows a dialog, where it would pick the highlighted option
// whatever was typed; held reports that.
func (m *manager) sendInput(ctx context.Context, ref, text string, submit, guard bool, keys []string) (held bool, err error) {
	session, adapterID, err := m.liveSession(ref)
	if err != nil {
		return false, err
	}
	if guard && submit && text != "" && m.dialogOpen(ctx, session, adapterID) {
		submit, held = false, true
	}
	if text != "" || submit {
		if err := m.d.tmux.SendText(ctx, session, text, submit); err != nil {
			return false, err
		}
	}
	return held, m.d.tmux.SendKeys(ctx, session, keys...)
}

// dialogOpen reports whether the agent's screen shows a dialog, as far as
// its adapter can tell (see adapter.DialogDetector).
func (m *manager) dialogOpen(ctx context.Context, session, adapterID string) bool {
	ad, _ := m.d.opts.Adapters.Get(adapterID)
	det, ok := ad.(adapter.DialogDetector)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()
	screen, err := m.d.tmux.Screen(ctx, session)
	return err == nil && det.DialogOpen(screen)
}
