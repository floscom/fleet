package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

// table writes aligned columns; call flush when done.
type table struct{ w *tabwriter.Writer }

func newTable(out io.Writer, header ...string) *table {
	t := &table{w: tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)}
	t.row(header...)
	return t
}

func (t *table) row(cols ...string) { fmt.Fprintln(t.w, strings.Join(cols, "\t")) }
func (t *table) flush()             { _ = t.w.Flush() }

// stateName is the human-friendly agent state.
func stateName(s fleetv1.AgentState) string {
	switch s {
	case fleetv1.AgentState_AGENT_STATE_STARTING:
		return "starting"
	case fleetv1.AgentState_AGENT_STATE_RUNNING:
		return "running"
	case fleetv1.AgentState_AGENT_STATE_WORKING:
		return "working"
	case fleetv1.AgentState_AGENT_STATE_IDLE:
		return "idle"
	case fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT:
		return "needs input"
	case fleetv1.AgentState_AGENT_STATE_EXITED:
		return "exited"
	case fleetv1.AgentState_AGENT_STATE_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

// agentState is stateName plus the exit code where known.
func agentState(a *fleetv1.Agent) string {
	s := stateName(a.State)
	if a.State == fleetv1.AgentState_AGENT_STATE_EXITED && a.HasExitCode {
		s += fmt.Sprintf(" (%d)", a.ExitCode)
	}
	return s
}

// age renders the time since ms (Unix milliseconds) compactly: 45s, 12m, 3h, 2d.
func age(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return compactDuration(time.Since(time.UnixMilli(ms)))
}

func compactDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortID abbreviates a long hex id for tables.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// fingerprint groups a hex id for reading aloud: "ab12 cd34 ...".
func fingerprint(id string) string {
	var b strings.Builder
	for i, r := range id {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
