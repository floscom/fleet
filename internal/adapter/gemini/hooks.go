package gemini

import (
	"encoding/json"
	"strings"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
)

// payload is the union of the Gemini hook input fields fleet reads.
type payload struct {
	SessionID        string `json:"session_id"`
	TranscriptPath   string `json:"transcript_path"`
	Source           string `json:"source"`
	NotificationType string `json:"notification_type"`
	Message          string `json:"message"`
	Details          struct {
		Title    string `json:"title"`
		Command  string `json:"command"`
		FileName string `json:"fileName"`
		ToolName string `json:"toolName"`
	} `json:"details"`
}

// HandleHook maps Gemini hook events to agent states (0.66.0):
//
//	SessionStart                    -> IDLE (records session_id)
//	BeforeAgent (a prompt), AfterTool -> WORKING
//	Notification ToolPermission     -> NEEDS_INPUT (what it asks to run)
//	AfterAgent (the turn's reply)   -> IDLE
//
// Notification names no tool call, so the dialog it opens has no Call:
// the next event of the session closes it. A tool refused with Esc, or a
// turn interrupted, fires no event; the dialog then closes on the screen
// (DialogOpen). Hook updates record the session's transcript_path, except
// SessionStart's on a resume, which names a file Gemini never writes.
func (g *gemini) HandleHook(ev adapter.HookEvent) (adapter.StateUpdate, bool) {
	var p payload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &p)
	}
	u := adapter.StateUpdate{SessionID: p.SessionID, Transcript: p.TranscriptPath}
	switch ev.Event {
	case "SessionStart":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		if p.Source == "resume" {
			u.Transcript = ""
		}
	case "BeforeAgent", "AfterTool":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "Notification":
		if p.NotificationType != "ToolPermission" {
			return adapter.StateUpdate{}, false
		}
		u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
		u.Detail = permissionDetail(p)
	case "AfterAgent":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	default:
		return adapter.StateUpdate{}, false
	}
	return u, true
}

// permissionDetail sums up what a ToolPermission notification asks, e.g.
// "Confirm Shell Command: touch c.txt".
func permissionDetail(p payload) string {
	what := p.Details.Command
	for _, s := range []string{p.Details.FileName, p.Details.ToolName} {
		if what == "" {
			what = s
		}
	}
	switch {
	case p.Details.Title != "" && what != "" && !strings.Contains(p.Details.Title, what):
		return hookcmd.Truncate(p.Details.Title + ": " + what)
	case p.Details.Title != "":
		return hookcmd.Truncate(p.Details.Title)
	}
	return hookcmd.Truncate(p.Message)
}
