package claude

import (
	"encoding/json"
	"os"
	"path/filepath"

	"fleet/internal/adapter/hookcmd"
)

// HookEvents are the Claude Code hook events fleet subscribes to.
var HookEvents = []string{
	"SessionStart",       // startup: session id, agent is idle
	"UserPromptSubmit",   // a turn starts
	"PostToolUse",        // back to work after a permission prompt
	"PostToolUseFailure", // a tool failed or the user interrupted the turn
	"PermissionDenied",   // a permission request was denied
	"PermissionRequest",  // permission dialog shown
	"Notification",       // permission / idle / question notifications
	"Stop",               // turn finished
	"StopFailure",        // turn ended by an API error
	"SubagentStop",       // a subagent finished: its dialogs are gone
}

// hookTimeout (seconds) bounds a `fleet hook` run; the default is 60.
const hookTimeout = 10

// Settings is the subset of Claude Code's settings.json that fleet writes:
//
//	{"hooks": {"<Event>": [{"matcher": "", "hooks": [{"type": "command", "command": "...", "timeout": 10}]}]}}
//
// An empty matcher matches everything; events without matchers ignore it.
type Settings struct {
	Hooks map[string][]MatcherGroup `json:"hooks"`
}

// MatcherGroup is one entry of a hook event list.
type MatcherGroup struct {
	Matcher string        `json:"matcher"`
	Hooks   []HookHandler `json:"hooks"`
}

// HookHandler is a single hook command.
type HookHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// BuildSettings returns the settings that route every HookEvents event to
// `fleet hook --agent <agentID> --adapter claude <Event>`.
func BuildSettings(fleetBinary, agentID string) Settings {
	s := Settings{Hooks: map[string][]MatcherGroup{}}
	for _, ev := range HookEvents {
		s.Hooks[ev] = []MatcherGroup{{Hooks: []HookHandler{{
			Type:    "command",
			Command: hookcmd.Command(fleetBinary, agentID, ID, ev),
			Timeout: hookTimeout,
		}}}}
	}
	return s
}

// writeSettings writes BuildSettings to path atomically (mode 0600).
func writeSettings(path, fleetBinary, agentID string) error {
	data, err := json.MarshalIndent(BuildSettings(fleetBinary, agentID), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".claude-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
