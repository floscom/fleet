// Package hookcmd holds helpers shared by adapters that install hooks: it
// builds the `fleet hook` command line agent CLIs run, and summarizes hook
// payloads for StateUpdate.Detail.
package hookcmd

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Command returns the shell command
//
//	'<fleetBinary>' hook --agent '<agentID>' --adapter '<adapterID>' '<event>'
//
// Agent CLIs run hook commands through a shell, so every word is quoted.
// The hook payload arrives on the command's stdin.
func Command(fleetBinary, agentID, adapterID, event string) string {
	return command(fleetBinary, "hook", "--agent", agentID, "--adapter", adapterID, event)
}

// WaitCommand is Command with --wait: the hook waits for the user's answer
// to what it asks, and prints it (see adapter.Asker).
func WaitCommand(fleetBinary, agentID, adapterID, event string) string {
	return command(fleetBinary, "hook", "--wait", "--agent", agentID, "--adapter", adapterID, event)
}

func command(words ...string) string {
	for i, w := range words {
		words[i] = Quote(w)
	}
	return strings.Join(words, " ")
}

// Quote returns s as a single POSIX shell word. Words made only of safe
// characters are returned unchanged.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=+,@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxDetail bounds StateUpdate.Detail strings derived from hook payloads.
const maxDetail = 200

// ToolDetail summarizes a tool call for StateUpdate.Detail, e.g.
// "Bash: rm -rf build". input is the tool's JSON arguments; the first of
// command, file_path, url, description or pattern is used.
func ToolDetail(tool string, input json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	for _, k := range []string{"command", "file_path", "url", "description", "pattern"} {
		if v, ok := args[k].(string); ok && v != "" {
			if tool == "" {
				return Truncate(v)
			}
			return Truncate(tool + ": " + v)
		}
	}
	return Truncate(tool)
}

// Truncate shortens s to a single line of at most 200 bytes, cutting on a
// rune boundary.
func Truncate(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) <= maxDetail {
		return s
	}
	cut := maxDetail - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
