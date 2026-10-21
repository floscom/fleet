package daemon

// Keeping the machine awake while agents have work to do: a laptop that
// goes to sleep on its idle timer stops every agent mid-turn, and one
// waiting for a usage limit to reset is never resumed. On macOS the daemon
// holds a caffeinate power assertion while any agent has work to do
// (stay_awake in config.toml).

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/workflow"
)

// Variables for tests.
var (
	// awakeInterval is how often the daemon looks whether to keep the
	// machine awake; macOS's shortest idle sleep timer is a minute.
	awakeInterval = 10 * time.Second
	// awakeCommand returns the command that keeps the machine awake while
	// it runs, nil where there is none.
	awakeCommand = caffeinate
)

const (
	// awakeResumeWithin: an agent waiting for its usage limit to reset
	// keeps the machine awake when it is resumed within this time (a
	// weekly limit may be days away).
	awakeResumeWithin = 6 * time.Hour
	// awakeStaleRun is how long a background workflow run may show no
	// activity and still keep the machine awake: one whose agents died
	// with the session never ends.
	awakeStaleRun = 30 * time.Minute
)

// caffeinate keeps a Mac from sleeping on its idle timer (-i) and, on AC
// power, at all (-s), until the daemon exits (-w).
func caffeinate() *exec.Cmd {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return exec.Command("/usr/bin/caffeinate", "-i", "-s", "-w", strconv.Itoa(os.Getpid()))
}

// awakeLoop keeps the machine awake while awakeReason says so, until ctx
// is cancelled.
func (m *manager) awakeLoop(ctx context.Context) {
	var cmd *exec.Cmd
	var exited chan struct{}
	release := func() {
		if cmd == nil {
			return
		}
		_ = cmd.Process.Kill()
		<-exited
		cmd, exited = nil, nil
		m.d.log.Info("letting the machine sleep")
	}
	defer release()
	t := time.NewTicker(awakeInterval)
	defer t.Stop()
	for {
		if exited != nil {
			select {
			case <-exited:
				m.d.log.Warn("keep-awake command ended", "err", cmd.ProcessState)
				cmd, exited = nil, nil
			default:
			}
		}
		why := ""
		if m.d.config().StayAwakeOn() {
			why = m.awakeReason(time.Now())
		}
		switch {
		case why != "" && cmd == nil:
			c := awakeCommand()
			if c == nil {
				return
			}
			if err := c.Start(); err != nil {
				m.d.log.Warn("cannot keep the machine awake", "cmd", c.Path, "err", err)
				return
			}
			cmd, exited = c, make(chan struct{})
			go func(c *exec.Cmd, exited chan struct{}) {
				_ = c.Wait()
				close(exited)
			}(cmd, exited)
			m.d.log.Info("keeping the machine awake", "reason", why)
		case why == "":
			release()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// awakeReason says why the machine must stay awake now: an agent starts or
// works, waits for its usage limit to reset to be resumed soon, or has
// workflows running in the background of its idle session. "" if nothing
// does.
func (m *manager) awakeReason(now time.Time) string {
	var idle []string
	m.mu.Lock()
	for _, a := range m.sortedLocked() {
		name := a.Name
		if name == "" {
			name = a.ID
		}
		switch at := a.limit.GetResumeAtMs(); {
		case a.State == fleetv1.AgentState_AGENT_STATE_STARTING || a.State == fleetv1.AgentState_AGENT_STATE_WORKING:
			m.mu.Unlock()
			return name + " works"
		case a.live() && at != 0 && time.UnixMilli(at).Sub(now) < awakeResumeWithin:
			m.mu.Unlock()
			return name + " is resumed at " + time.UnixMilli(at).Format(time.TimeOnly)
		case a.State == fleetv1.AgentState_AGENT_STATE_IDLE:
			idle = append(idle, a.ID)
		}
	}
	m.mu.Unlock()
	since := now.Add(-awakeStaleRun).UnixMilli()
	for _, id := range idle {
		ag, runs, err := m.workflows(id)
		if err != nil {
			continue
		}
		for _, r := range runs {
			if r.Status == workflow.Running && max(r.UpdatedMs, r.StartedMs) >= since {
				name := ag.GetName()
				if name == "" {
					name = id
				}
				return name + " runs workflow " + r.ID
			}
		}
	}
	return ""
}
