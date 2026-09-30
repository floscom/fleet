package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
)

// hookEvent is a Codex hook event and its snake_case key in hook trust ids.
type hookEvent struct{ Name, Key string }

// hookEvents are the Codex hook events fleet subscribes to.
var hookEvents = []hookEvent{
	{"SessionStart", "session_start"},           // first turn: session id
	{"UserPromptSubmit", "user_prompt_submit"},  // a turn starts
	{"PermissionRequest", "permission_request"}, // approval requested
	{"PostToolUse", "post_tool_use"},            // back to work after approval
	{"Stop", "stop"},                            // turn finished
}

// hookTimeout (seconds) bounds a `fleet hook` run; the default is 600.
const hookTimeout = 10

// sessionFlagsSource is how Codex names the -c override layer in hook trust
// keys (observed in 0.154: "/<session-flags>/config.toml:session_start:0:0").
const sessionFlagsSource = "/<session-flags>/config.toml"

// HookOverrides returns the -c values that install fleet's hooks and mark
// them trusted:
//
//	hooks.Stop=[{hooks=[{type="command",command="...",timeout=10}]}]
//	hooks.state={"/<session-flags>/config.toml:stop:0:0"={trusted_hash="sha256:..."}, ...}
func HookOverrides(fleetBinary, agentID string) []string {
	var out, state []string
	for _, ev := range hookEvents {
		cmd := hookcmd.Command(fleetBinary, agentID, ID, ev.Name)
		out = append(out, fmt.Sprintf(`hooks.%s=[{hooks=[{type="command",command=%s,timeout=%d}]}]`,
			ev.Name, tomlString(cmd), hookTimeout))
		key := fmt.Sprintf("%s:%s:0:0", sessionFlagsSource, ev.Key)
		state = append(state, fmt.Sprintf(`%s={trusted_hash=%s}`, tomlString(key), tomlString(TrustHash(ev.Key, cmd))))
	}
	return append(out, "hooks.state={"+strings.Join(state, ",")+"}")
}

// TrustHash reproduces Codex's hook fingerprint for a single command hook
// without matcher: "sha256:" + hex SHA-256 of the canonical (sorted keys,
// compact) JSON of the normalized hook identity
//
//	{"event_name":"<key>","hooks":[{"async":false,"command":"...","timeout":10,"type":"command"}]}
func TrustHash(eventKey, command string) string {
	return trustHash(eventKey, command, hookTimeout)
}

func trustHash(eventKey, command string, timeout int) string {
	identity := map[string]any{
		"event_name": eventKey,
		"hooks": []any{map[string]any{
			"async":   false,
			"command": command,
			"timeout": timeout,
			"type":    "command",
		}},
	}
	sum := sha256.Sum256(canonicalJSON(identity))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalJSON encodes v like serde_json: map keys sorted (encoding/json
// does that), no HTML escaping, U+2028/U+2029 left unescaped.
func canonicalJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only strings, bools and numbers
	}
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte(`\u2028`), []byte("\u2028"))
	return bytes.ReplaceAll(out, []byte(`\u2029`), []byte("\u2029"))
}

// tomlString quotes s as a TOML basic string. JSON string escapes are a
// subset of TOML's.
func tomlString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// payload is the union of the Codex hook input fields fleet reads (JSON on
// stdin), plus the legacy notify payload (JSON as the last argv argument).
type payload struct {
	SessionID string          `json:"session_id"`
	Source    string          `json:"source"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// TranscriptPath may be null; it then stays empty.
	TranscriptPath string `json:"transcript_path"`

	// notify
	Type     string `json:"type"`
	ThreadID string `json:"thread-id"`
}

// HandleHook maps Codex hook events to agent states:
//
//	SessionStart                   -> WORKING (records session_id; Codex
//	                                  starts its session with the first turn)
//	SessionStart source "clear"    -> IDLE
//	UserPromptSubmit, PostToolUse  -> WORKING
//	PermissionRequest              -> NEEDS_INPUT (tool summary)
//	Stop                           -> IDLE
//	notify agent-turn-complete     -> IDLE
//	notify approval-*              -> NEEDS_INPUT
//
// Hook updates record the session's transcript_path.
func (c *codex) HandleHook(ev adapter.HookEvent) (adapter.StateUpdate, bool) {
	var p payload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &p)
	}
	u := adapter.StateUpdate{SessionID: p.SessionID, Transcript: p.TranscriptPath}
	switch ev.Event {
	case "SessionStart":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
		if p.Source == "clear" {
			u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		}
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		u.State = fleetv1.AgentState_AGENT_STATE_WORKING
	case "PermissionRequest":
		u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
		u.Detail = hookcmd.ToolDetail(p.ToolName, p.ToolInput)
	case "Stop":
		u.State = fleetv1.AgentState_AGENT_STATE_IDLE
	case "notify":
		u.SessionID = p.ThreadID
		switch {
		case p.Type == "agent-turn-complete":
			u.State = fleetv1.AgentState_AGENT_STATE_IDLE
		case strings.HasPrefix(p.Type, "approval"):
			u.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
		default:
			return adapter.StateUpdate{}, false
		}
	default:
		return adapter.StateUpdate{}, false
	}
	return u, true
}
