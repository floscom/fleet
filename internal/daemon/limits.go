package daemon

// Agents stopped by a usage limit of their account (adapter.Limiter): the
// daemon follows each session's transcript for a turn the limit stopped,
// finds out when the limit resets (from the transcript, else from the
// account's usage, adapter.Usager), and then types a message into the
// session to resume it ([limits] in config.toml, switched per agent from
// the dashboard).

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

// Variables for tests.
var (
	// limitInterval is how often live sessions are checked for a limit.
	limitInterval = 5 * time.Second
	// resumeMargin is how long after a limit's reset the session is
	// resumed: the provider's clock and ours may differ a little.
	resumeMargin = time.Minute
)

const (
	// unknownWait is how long to wait before looking again when nobody
	// says when the limit resets.
	unknownWait = 10 * time.Minute
	// retryAfter is how long a resume may take to show in the transcript
	// before it is tried again, at most maxTries times.
	retryAfter = 3 * time.Minute
	maxTries   = 3
	// More than maxResumes resumes within resumeWindow: the limit keeps
	// coming back (the reset time is wrong, or another limit ran out).
	maxResumes   = 4
	resumeWindow = time.Hour
	// dialogClose is how long a dialog gets to close after Escape.
	dialogClose = time.Second
)

// limitWatch follows one agent's session for usage limits. Its mu is held
// while the agent is checked or resumed.
type limitWatch struct {
	mu sync.Mutex
	// path and off: how far the transcript was read; l is what it says.
	path string
	off  int64
	l    adapter.Limit
	// screenN is how many stopped turns the screen showed when last looked
	// (adapter.ScreenLimiter); fromScreen is set when l.Hit came from it.
	screenN    int
	fromScreen bool

	// The current stop: since when, when to resume, how often that was
	// tried, why it gave up. force skips the checks once (resume now).
	since    time.Time
	resumeAt time.Time
	tries    int
	gaveUp   string
	force    bool
	// resumes are the times the agent was resumed, for maxResumes.
	resumes []time.Time
}

// limitLoop checks live agents for usage limits until ctx is cancelled.
func (m *manager) limitLoop(ctx context.Context) {
	t := time.NewTicker(limitInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.watchLimits(ctx)
	}
}

// watchLimits checks every live agent whose adapter is a Limiter, and
// forgets the watches of agents that ended.
func (m *manager) watchLimits(ctx context.Context) {
	var ids []string
	m.mu.Lock()
	for id, a := range m.agents {
		ad, _ := m.d.opts.Adapters.Get(a.Adapter)
		if _, ok := ad.(adapter.Limiter); ok && a.live() && !a.busy {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	m.limitMu.Lock()
	for id := range m.limits {
		if !slices.Contains(ids, id) {
			delete(m.limits, id)
		}
	}
	m.limitMu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		m.watchLimit(ctx, id)
	}
}

// limitWatch returns the limit watch of agent id.
func (m *manager) limitWatch(id string) *limitWatch {
	m.limitMu.Lock()
	defer m.limitMu.Unlock()
	w := m.limits[id]
	if w == nil {
		if m.limits == nil {
			m.limits = map[string]*limitWatch{}
		}
		w = &limitWatch{}
		m.limits[id] = w
	}
	return w
}

// watchLimit checks one agent: whether its last turn was stopped by a
// usage limit, when that resets, and resumes it when due.
func (m *manager) watchLimit(ctx context.Context, id string) {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok || !a.live() || a.busy {
		m.mu.Unlock()
		return
	}
	adapterID, session, state := a.Adapter, a.TmuxSession, a.State
	auto := m.autoResumeLocked(a)
	m.mu.Unlock()
	ad, _ := m.d.opts.Adapters.Get(adapterID)
	lim, ok := ad.(adapter.Limiter)
	if !ok {
		return
	}
	w := m.limitWatch(id)
	w.mu.Lock()
	defer w.mu.Unlock()

	if s, err := m.session(id); err == nil {
		w.follow(lim, s.path)
	}
	if sl, ok := ad.(adapter.ScreenLimiter); ok {
		if screen, err := (terminal{m.d.tmux, session}).Screen(ctx); err == nil {
			n := sl.LimitsOnScreen(screen)
			if n > w.screenN && !w.l.Hit {
				w.l.Hit, w.fromScreen = true, true
			}
			w.screenN = n
		}
	}
	if !w.l.Hit {
		w.since, w.resumeAt, w.tries, w.gaveUp, w.force, w.fromScreen = time.Time{}, time.Time{}, 0, "", false, false
		m.showLimit(id, nil)
		return
	}

	now := time.Now()
	usager, _ := ad.(adapter.Usager)
	// The account's usage, asked only when needed (cached for usageTTL; unknown while it cannot be renewed).
	var usage *usageEntry
	exhausted := func() (window string, resets time.Time, known bool) {
		if usager == nil {
			return "", time.Time{}, false
		}
		if usage == nil {
			e := m.d.usage.get(ctx, adapterID, usager)
			usage = &e
		}
		if usage.err != nil || usage.usage == nil {
			return "", time.Time{}, false
		}
		return exhaustedWindow(usage.usage, w.l.Window)
	}

	if w.fromScreen {
		// The screen may show an old stop; the account must agree.
		if win, _, known := exhausted(); known && win == "" {
			w.l.Hit, w.fromScreen = false, false
			m.showLimit(id, nil)
			return
		}
	}
	if w.since.IsZero() {
		w.since, w.resumeAt, w.tries, w.gaveUp = now, time.Time{}, 0, ""
	}
	window, resets := w.l.Window, w.l.ResetsAt
	if resets.IsZero() {
		if win, r, _ := exhausted(); win != "" {
			window, resets = win, r
		}
	}
	if w.resumeAt.IsZero() {
		w.resumeAt = resumeTime(resets, now)
	}

	// force (resume now) resumes once, whether auto-resume is on or not.
	if (auto && w.gaveUp == "" || w.force) && !now.Before(w.resumeAt) {
		m.tryResume(ctx, w, id, adapterID, session, state, exhausted, now)
	}

	ul := &fleetv1.UsageLimit{Window: window, AutoResume: auto, Detail: w.gaveUp}
	if !resets.IsZero() {
		ul.ResetsAtMs = resets.UnixMilli()
	}
	if auto && w.gaveUp == "" {
		ul.ResumeAtMs = w.resumeAt.UnixMilli()
	}
	m.showLimit(id, ul)
}

// tryResume resumes the agent, unless the limit has not reset yet or
// resuming keeps failing.
func (m *manager) tryResume(ctx context.Context, w *limitWatch, id, adapterID, session string, state fleetv1.AgentState,
	exhausted func() (string, time.Time, bool), now time.Time) {
	force := w.force
	w.force = false
	if !force {
		if win, r, _ := exhausted(); win != "" {
			// Not reset yet, whatever the transcript said.
			w.resumeAt = resumeTime(r, now)
			if !r.After(now) {
				w.resumeAt = now.Add(unknownWait)
			}
			return
		}
		if state == stateWorking {
			// It goes on by itself (Claude Code can wait out a limit); the
			// transcript shows that shortly.
			w.resumeAt = now.Add(retryAfter)
			return
		}
		w.resumes = slices.DeleteFunc(w.resumes, func(t time.Time) bool { return now.Sub(t) > resumeWindow })
		switch {
		case w.tries >= maxTries:
			w.gaveUp = fmt.Sprintf("auto-resume gave up: no reaction to %d tries", w.tries)
			return
		case len(w.resumes) >= maxResumes:
			w.gaveUp = fmt.Sprintf("auto-resume gave up: stopped by a limit %d times within an hour", len(w.resumes))
			return
		}
	}
	msg := m.d.config().Limits.MessageOrDefault()
	if err := m.resumeSession(ctx, session, adapterID, msg); err != nil {
		m.d.log.Warn("resuming after usage limit failed", "agent", id, "err", err)
		w.gaveUp = "auto-resume failed: " + err.Error()
		return
	}
	m.d.log.Info("resumed after usage limit", "agent", id)
	w.tries++
	w.resumes = append(w.resumes, now)
	w.resumeAt = now.Add(retryAfter)
}

// resumeSession types msg into a session and presses Enter, after closing
// a dialog the CLI shows (Claude Code's menu of what to do about the limit):
// Enter would pick its highlighted option.
func (m *manager) resumeSession(ctx context.Context, session, adapterID, msg string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*screenTimeout+dialogClose)
	defer cancel()
	if m.dialogOpen(ctx, session, adapterID) {
		if err := m.d.tmux.SendKeys(ctx, session, "Escape"); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dialogClose):
		}
		if m.dialogOpen(ctx, session, adapterID) {
			return fmt.Errorf("a dialog is open in its terminal")
		}
	}
	return m.d.tmux.SendText(ctx, session, msg, true)
}

// resumeTime is when to resume a session stopped by a limit that resets
// at resets (zero: unknown).
func resumeTime(resets, now time.Time) time.Time {
	if resets.IsZero() {
		return now.Add(unknownWait)
	}
	return resets.Add(resumeMargin)
}

// exhaustedWindow returns the window of u that is used up, preferring the
// one named want, else the one that resets last. known is false when u
// has no windows at all.
func exhaustedWindow(u *adapter.Usage, want string) (window string, resets time.Time, known bool) {
	for _, l := range u.Limits {
		if l.Percent < 100 {
			continue
		}
		if l.Label == want {
			return l.Label, l.ResetsAt, true
		}
		if window == "" || l.ResetsAt.After(resets) {
			window, resets = l.Label, l.ResetsAt
		}
	}
	return window, resets, len(u.Limits) > 0
}

// follow reads what was appended to the transcript at path ("" while there
// is none) since the last call. A new transcript (a new session) starts
// with no limit.
func (w *limitWatch) follow(lim adapter.Limiter, path string) {
	if path == "" {
		return
	}
	if w.path != path {
		w.path, w.off, w.l = path, 0, adapter.Limit{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Size() < w.off {
		w.off, w.l = 0, adapter.Limit{}
	}
	visit := func(line []byte) []transcript.Entry {
		lim.LimitLine(line, &w.l)
		return nil
	}
	for w.off < fi.Size() {
		p, err := transcript.Forward(path, w.off, visit)
		if err != nil || p.End == w.off {
			return
		}
		w.off = p.End
		if !p.More {
			return
		}
	}
}

// showLimit sets the usage limit agent id shows (nil: none). An agent the
// limit stopped is idle: Codex reports no end of a turn that failed.
func (m *manager) showLimit(id string, ul *fleetv1.UsageLimit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok || !a.live() || proto.Equal(a.limit, ul) {
		return
	}
	stopped := a.limit == nil && ul != nil
	a.limit = ul
	if stopped {
		if a.OwnState == 0 && a.State != stateNeedsInput {
			a.OwnState, a.OwnDetail = a.State, a.StateDetail
		}
		switch a.OwnState {
		case stateWorking, stateRunning, stateIdle:
			a.OwnState, a.OwnDetail = stateIdle, limitDetail(ul)
		}
		a.showDialogs()
	}
	m.changedLocked(a)
}

// limitDetail is the state detail of an agent stopped by ul.
func limitDetail(ul *fleetv1.UsageLimit) string {
	if ul.GetWindow() != "" {
		return "Usage limit reached (" + ul.GetWindow() + ")"
	}
	return "Usage limit reached"
}

// autoResumeLocked reports whether agent a is resumed after a usage limit.
func (m *manager) autoResumeLocked(a *agentRec) bool {
	if a.AutoResume != nil {
		return *a.AutoResume
	}
	return m.d.config().Limits.AutoResumeOn()
}

// setAutoResume switches auto-resume for an agent and/or resumes it now
// (now: the limit is said to have reset), and returns the agent.
func (m *manager) setAutoResume(ctx context.Context, ref string, auto *bool, now bool) (*fleetv1.Agent, error) {
	m.mu.Lock()
	a, err := m.find(ref)
	if err == nil && !a.live() {
		err = errf(codeInvalid, "agent %s is not running", a.ID)
	}
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	id := a.ID
	if now && a.limit == nil {
		m.mu.Unlock()
		return nil, errf(codeInvalid, "%s was not stopped by a usage limit", a.Name)
	}
	if auto != nil {
		v := *auto
		a.AutoResume = &v
		m.saveLocked()
	}
	m.mu.Unlock()

	w := m.limitWatch(id)
	w.mu.Lock()
	if auto != nil && *auto {
		w.gaveUp, w.tries = "", 0
	}
	if now {
		w.gaveUp, w.force, w.resumeAt = "", true, time.Now()
	}
	w.mu.Unlock()
	m.watchLimit(ctx, id)

	m.mu.Lock()
	defer m.mu.Unlock()
	a, err = m.find(id)
	if err != nil {
		return nil, err
	}
	return a.proto(), nil
}
