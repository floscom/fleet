package claude

import (
	"encoding/json"
	"strings"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
)

// payload is the union of the Claude Code hook input fields fleet reads.
// Every hook receives it as JSON on stdin.
type payload struct {
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	Source           string          `json:"source"`
	Message          string          `json:"message"`
	NotificationType string          `json:"notification_type"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	Error            json.RawMessage `json:"error"` // string or object
	IsInterrupt      bool            `json:"is_interrupt"`
	AgentID          string          `json:"agent_id"` // set inside subagents
}

// HandleHook maps Claude Code hook events to agent states:
//
//	SessionStart                      -> IDLE (records session_id)
//	UserPromptSubmit, PostToolUse     -> WORKING
//	PermissionRequest                 -> NEEDS_INPUT (tool summary)
//	Notification idle_prompt          -> IDLE
//	Notification permission_prompt    -> ignored (see below)
//	Notification (other)              -> NEEDS_INPUT (message)
//	Stop, StopFailure                 -> IDLE (StopFailure: the error as detail)
//	PostToolUseFailure is_interrupt   -> IDLE (Esc: no Stop hook follows)
//	PostToolUseFailure, PermissionDenied -> WORKING (the turn goes on)
//	SubagentStop                      -> WORKING, from that subagent
//
// Informational notifications (auth_success, elicitation results) are ignored,
// and so is permission_prompt: PermissionRequest reports the same dialog
// first, with the tool and the subagent asking, which the notification
// lacks. Every update records the session's transcript_path, and names the
// subagent it came from and the tool call it is about (StateUpdate.Subagent
// and Call).
func (c *claude) HandleHook(ev adapter.HookEvent) (adapter.StateUpdate, bool) {
	var p payload
	if len(ev.Payload) > 0 {
		// A malformed payload still carries the event name; map what we can.
		_ = json.Unmarshal(ev.Payload, &p)
	}
	u := adapter.StateUpdate{SessionID: p.SessionID, Transcript: p.TranscriptPath, Subagent: p.AgentID}
	switch ev.Event {
	case "SessionStart":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	case "UserPromptSubmit":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "PreToolUse", "PostToolUse":
		u.State, u.Call = fleetv1.AgentState_AGENT_STATE_WORKING, callSummary(p)
	case "PermissionRequest":
		u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
		u.Detail = callSummary(p)
		u.Call = u.Detail
	case "Notification":
		switch {
		case isIdleNotification(p):
			u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		case p.NotificationType == "auth_success", p.NotificationType == "permission_prompt",
			strings.HasPrefix(p.NotificationType, "elicitation_") && p.NotificationType != "elicitation_dialog":
			return adapter.StateUpdate{}, false
		default:
			u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
			u.Detail = hookcmd.Truncate(p.Message)
		}
	case "PostToolUseFailure":
		// An interrupt (Esc) ends the whole turn, not just this call.
		u.State, u.Call = fleetv1.AgentState_AGENT_STATE_WORKING, callSummary(p)
		if p.IsInterrupt {
			u.State, u.Call = fleetv1.AgentState_AGENT_STATE_IDLE, ""
		}
	case "PermissionDenied":
		u.State, u.Call = fleetv1.AgentState_AGENT_STATE_WORKING, callSummary(p)
	case "SubagentStop":
		// Only closes a dialog the subagent left open: a permission it
		// was refused ends in no hook of its own.
		if p.AgentID == "" {
			return adapter.StateUpdate{}, false
		}
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "Stop":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	case "StopFailure":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		u.Detail = "turn ended with an API error"
		var msg string
		switch {
		case json.Unmarshal(p.Error, &msg) != nil || msg == "":
		case msg == "rate_limit":
			u.Detail = "Usage limit reached"
		default:
			u.Detail = hookcmd.Truncate(msg)
		}
	default:
		return adapter.StateUpdate{}, false
	}
	return u, true
}

// callSummary summarizes the tool call of p, e.g. "Bash: npm test". It is
// the detail of a permission dialog, and ties the dialog to the later
// events of the call: PermissionRequest carries no tool_use_id. For
// AskUserQuestion, whose dialog asks questions, it is the first question.
func callSummary(p payload) string {
	if p.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if json.Unmarshal(p.ToolInput, &in) == nil && len(in.Questions) > 0 && in.Questions[0].Question != "" {
			return hookcmd.Truncate("Claude asks: " + in.Questions[0].Question)
		}
	}
	return hookcmd.ToolDetail(p.ToolName, p.ToolInput)
}

// isIdleNotification reports whether p is the "Claude is waiting for your
// input" reminder sent after a turn has been idle for a while. Older
// versions send no notification_type, so the message is checked too.
func isIdleNotification(p payload) bool {
	if p.NotificationType != "" {
		return p.NotificationType == "idle_prompt"
	}
	return strings.Contains(strings.ToLower(p.Message), "waiting for your input")
}
