package daemon

import (
	"context"
	"os"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

// Variables for tests.
var (
	// interruptWait is how long an interrupted turn may take to show in
	// the transcript.
	interruptWait = 3 * time.Second
	// interruptPoll is how often the transcript is read meanwhile.
	interruptPoll = 100 * time.Millisecond
)

// interruptKey interrupts a turn in the CLIs fleet drives (Claude Code,
// Codex), and closes the dialog they show.
const interruptKey = "Escape"

// interrupt interrupts the turn a live agent is working on, as Esc in its
// terminal does, and reports whether its transcript recorded that. No hook
// reports a turn interrupted while the model writes, so the agent is made
// idle here once the transcript shows it.
//
// The agent must be working or wait on a dialog: Esc twice at an idle
// Claude prompt opens its rewind menu.
func (m *manager) interrupt(ctx context.Context, ref string) (*fleetv1.Agent, bool, error) {
	m.mu.Lock()
	a, err := m.find(ref)
	if err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	if !a.live() {
		m.mu.Unlock()
		return nil, false, errf(codeInvalid, "agent %s is not running", a.ID)
	}
	if a.State != stateWorking && a.State != stateNeedsInput {
		m.mu.Unlock()
		return nil, false, errf(codeInvalid, "agent %s is not working", a.ID)
	}
	id, session := a.ID, a.TmuxSession
	// The turn ends now: the next queued message would undo the Stop.
	m.pauseQueueLocked(a)
	m.mu.Unlock()

	// Where the transcript ends now: the interruption is recorded after.
	s, _ := m.session(id)
	off := int64(-1)
	if s.t != nil && s.path != "" {
		if fi, err := os.Stat(s.path); err == nil {
			off = fi.Size()
		}
	}
	if err := m.d.tmux.SendKeys(ctx, session, interruptKey); err != nil {
		return nil, false, err
	}
	done := off >= 0 && interrupted(ctx, s.path, off, s.t.ParseTranscript)

	m.mu.Lock()
	defer m.mu.Unlock()
	a, err = m.find(id)
	if err != nil {
		return nil, false, err
	}
	if done && a.live() {
		ad, _ := m.d.opts.Adapters.Get(a.Adapter)
		m.applyHookLocked(a, ad, adapter.StateUpdate{State: stateIdle})
	}
	return a.proto(), done, nil
}

// interrupted waits, at most interruptWait, for the transcript at path to
// record an interrupted turn after offset off.
func interrupted(ctx context.Context, path string, off int64, parse transcript.Parser) bool {
	ctx, cancel := context.WithTimeout(ctx, interruptWait)
	defer cancel()
	t := time.NewTicker(interruptPoll)
	defer t.Stop()
	for {
		for {
			p, err := transcript.Forward(path, off, parse)
			if err != nil {
				return false
			}
			for _, e := range p.Entries {
				if e.Kind == transcript.Note && e.Text == transcript.Interrupted {
					return true
				}
			}
			more := p.More && p.End > off
			off = p.End
			if !more {
				break
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}
