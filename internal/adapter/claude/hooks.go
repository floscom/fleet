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
	Source           string          `json:"source"`
	Message          string          `json:"message"`
	NotificationType string          `json:"notification_type"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	Error            json.RawMessage `json:"error"` // string or object
	IsInterrupt      bool            `json:"is_interrupt"`
}

// HandleHook maps Claude Code hook events to agent states:
//
//	SessionStart                      -> IDLE (records session_id)
//	UserPromptSubmit, PostToolUse     -> WORKING
//	PermissionRequest                 -> NEEDS_INPUT (tool summary)
//	Notification idle_prompt          -> IDLE
//	Notification (other)              -> NEEDS_INPUT (message)
//	Stop, StopFailure                 -> IDLE
//	PostToolUseFailure is_interrupt   -> IDLE (Esc: no Stop hook follows)
//	PostToolUseFailure, PermissionDenied -> WORKING (the turn goes on)
//
// Informational notifications (auth_success, elicitation results) are ignored.
func (c *claude) HandleHook(ev adapter.HookEvent) (adapter.StateUpdate, bool) {
	var p payload
	if len(ev.Payload) > 0 {
		// A malformed payload still carries the event name; map what we can.
		_ = json.Unmarshal(ev.Payload, &p)
	}
	u := adapter.StateUpdate{SessionID: p.SessionID}
	switch ev.Event {
	case "SessionStart":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "PermissionRequest":
		u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
		u.Detail = hookcmd.ToolDetail(p.ToolName, p.ToolInput)
	case "Notification":
		switch {
		case isIdleNotification(p):
			u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		case p.NotificationType == "auth_success",
			strings.HasPrefix(p.NotificationType, "elicitation_") && p.NotificationType != "elicitation_dialog":
			return adapter.StateUpdate{}, false
		default:
			u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
			u.Detail = hookcmd.Truncate(p.Message)
		}
	case "PostToolUseFailure":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
		if p.IsInterrupt {
			u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		}
	case "PermissionDenied":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "Stop":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	case "StopFailure":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		u.Detail = "turn ended with an API error"
		var msg string
		if json.Unmarshal(p.Error, &msg) == nil && msg != "" {
			u.Detail = hookcmd.Truncate(msg)
		}
	default:
		return adapter.StateUpdate{}, false
	}
	return u, true
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
