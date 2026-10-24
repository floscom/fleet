package daemon

// Keeping fleet up to date: the daemon looks for a newer release on GitHub
// a few minutes after it starts and every 6 hours, installs it over its
// own binary, and restarts on it once no agent works, so no turn, question
// or attached terminal is cut off. Agents keep running in tmux across the
// restart, as across `fleet stop && fleet start`. auto_update = false in
// config.toml (or `fleet update --auto off`) turns it off; `fleet update`
// still updates by hand. Development builds never update themselves.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/update"
)

// Variables for tests.
var (
	// updateFirst is how long after the start the first look happens.
	updateFirst = 5 * time.Minute
	// updateInterval is how often the daemon looks after that.
	updateInterval = 6 * time.Hour
	// updatePoll is how often an installed update looks whether the
	// daemon can restart on it.
	updatePoll = 30 * time.Second
	// updateSource is where releases come from.
	updateSource = update.Source{}
)

// ErrRestart is returned by Run when the daemon stopped to restart on an
// updated binary: the caller execs the binary again.
var ErrRestart = errors.New("restart on the updated fleet binary")

// updater is where the daemon stands with updates.
type updater struct {
	// run serializes looking and installing.
	run sync.Mutex
	// kick makes the loop look now (auto_update was turned on), or
	// restart now on an update installed by hand (see nudge).
	kick chan struct{}

	mu        sync.Mutex
	latest    string
	checkedAt time.Time
	installed string
	err       string
}

// updateUnsupported tells why this daemon cannot update itself, "" if it
// can.
func (d *daemon) updateUnsupported() string {
	if !update.Released(d.opts.Version) {
		return "this is a development build (" + d.opts.Version + "); only releases update themselves"
	}
	if err := update.Writable(d.opts.FleetBinary); err != nil {
		return err.Error()
	}
	return ""
}

// updateStatus reports where the daemon stands with updates.
func (d *daemon) updateStatus() *fleetv1.UpdateStatus {
	u := &d.upd
	u.mu.Lock()
	defer u.mu.Unlock()
	st := &fleetv1.UpdateStatus{
		Version: d.opts.Version, Auto: d.config().AutoUpdateOn(), Unsupported: d.updateUnsupported(),
		Latest: u.latest, Installed: u.installed, Error: u.err,
	}
	if !u.checkedAt.IsZero() {
		st.CheckedAtMs = u.checkedAt.UnixMilli()
	}
	return st
}

// checkUpdate looks for the newest release and, with install, installs it
// when it is newer than what runs (or was installed already). What went
// wrong ends up in the status.
func (d *daemon) checkUpdate(ctx context.Context, install bool) {
	u := &d.upd
	u.run.Lock()
	defer u.run.Unlock()
	tag, err := updateSource.Latest(ctx)
	u.mu.Lock()
	u.checkedAt = time.Now()
	if err != nil {
		u.err = err.Error()
	} else {
		u.latest, u.err = tag, ""
	}
	have := u.installed
	u.mu.Unlock()
	if err != nil {
		d.log.Warn("cannot look for fleet updates", "err", err)
		return
	}
	if have == "" {
		have = d.opts.Version
	}
	if !install || !update.Newer(tag, have) {
		return
	}
	d.log.Info("installing fleet update", "version", tag, "binary", d.opts.FleetBinary)
	err = updateSource.Install(ctx, tag, d.opts.FleetBinary)
	u.mu.Lock()
	if err != nil {
		u.err = err.Error()
	} else {
		u.installed = tag
	}
	u.mu.Unlock()
	if err != nil {
		d.log.Warn("fleet update failed", "version", tag, "err", err)
		return
	}
	d.log.Info("fleet update installed; restarting once no agent works", "version", tag)
}

// updateLoop looks for updates while auto_update is on, and restarts the
// daemon once an installed update can be, until ctx is cancelled.
func (d *daemon) updateLoop(ctx context.Context) {
	look := time.NewTimer(updateFirst)
	defer look.Stop()
	poll := time.NewTicker(updatePoll)
	defer poll.Stop()
	waiting := ""
	for {
		kicked := false
		select {
		case <-ctx.Done():
			return
		case <-look.C:
			look.Reset(updateInterval)
			if d.config().AutoUpdateOn() && d.updateUnsupported() == "" {
				d.checkUpdate(ctx, true)
			}
		case <-d.upd.kick:
			kicked = true
		case <-poll.C:
		}
		installed := d.upd.pending()
		if installed == "" && kicked && d.updateUnsupported() == "" {
			d.checkUpdate(ctx, true)
			installed = d.upd.pending()
		}
		if installed == "" {
			continue
		}
		if why := d.agents.busyReason(); why != "" {
			if why != waiting {
				d.log.Info("fleet update waits to restart", "version", installed, "reason", why)
				waiting = why
			}
			continue
		}
		d.log.Info("restarting on the fleet update", "version", installed)
		d.restart()
		return
	}
}

// handleUpdate answers an UpdateRequest.
func (d *daemon) handleUpdate(ctx context.Context, req *fleetv1.UpdateRequest) (*fleetv1.UpdateStatus, error) {
	if req.GetSetAuto() {
		on := req.GetAuto()
		d.cfgMu.Lock()
		old := d.cfg.AutoUpdate
		d.cfg.AutoUpdate = &on
		if err := d.cfg.Save(); err != nil {
			d.cfg.AutoUpdate = old
			d.cfgMu.Unlock()
			return nil, fmt.Errorf("save config: %w", err)
		}
		d.cfgMu.Unlock()
		d.log.Info("automatic updates", "on", on)
		if on && !req.GetCheck() && !req.GetInstall() {
			d.upd.nudge(time.Second)
		}
	}
	// A daemon that cannot update itself only looks; its status says why.
	if req.GetCheck() || req.GetInstall() {
		d.checkUpdate(ctx, req.GetInstall() && d.updateUnsupported() == "")
	}
	st := d.updateStatus()
	if req.GetInstall() && st.Installed != "" {
		// Restart without waiting for the next poll, but after this
		// reply went out.
		d.upd.nudge(time.Second)
	}
	return st, nil
}

// pending is the version installed and waiting for a restart, "" if none.
func (u *updater) pending() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.installed
}

// nudge kicks the update loop after delay.
func (u *updater) nudge(delay time.Duration) {
	time.AfterFunc(delay, func() {
		select {
		case u.kick <- struct{}{}:
		default:
		}
	})
}

// busyReason says why the daemon must not restart now: an agent starts
// or works, a hook waits for the answer to a question, or a terminal is
// attached through the daemon. "" if nothing does.
func (m *manager) busyReason() string {
	m.mu.Lock()
	for _, a := range m.sortedLocked() {
		if a.State == fleetv1.AgentState_AGENT_STATE_STARTING || a.State == fleetv1.AgentState_AGENT_STATE_WORKING {
			name := a.Name
			if name == "" {
				name = a.ID
			}
			m.mu.Unlock()
			return name + " works"
		}
	}
	asks := len(m.asks)
	m.mu.Unlock()
	if asks > 0 {
		return "a question waits for its answer"
	}
	d := m.d
	d.connsMu.Lock()
	defer d.connsMu.Unlock()
	for c := range d.conns {
		c.attachMu.Lock()
		n := len(c.attaches)
		c.attachMu.Unlock()
		if n > 0 {
			return "a terminal is attached"
		}
	}
	return ""
}
