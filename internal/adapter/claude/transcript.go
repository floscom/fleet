package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
	"fleet/internal/transcript"
)

// Claude Code (2.1.280) appends every event of a session as one JSON object
// per line to <config dir>/projects/<cwd, "/" and "." replaced by
// "-">/<session id>.jsonl. Subagents write to <session id>/subagents/ next
// to it; those files are not shown. Lines have a "type": "user" and
// "assistant" lines carry an API message, "system" lines notes such as a
// compacted conversation; everything else (attachment, queue-operation,
// last-prompt, cost-state, ...) is bookkeeping.

var _ adapter.Transcripter = (*claude)(nil)

// TranscriptDir implements adapter.Transcripter. Transcripts live in the
// config dir, like the credentials.
func (c *claude) TranscriptDir(home string) string {
	auth := c.AuthFile(home)
	if auth == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(auth), "projects")
}

// FindTranscript implements adapter.Transcripter: the session's file is in
// the project dir of the directory the agent started in. If several
// projects have it (a resumed session copied elsewhere), the most recently
// written wins.
func (c *claude) FindTranscript(dir, sessionID string) (string, bool) {
	if !validSessionID(sessionID) {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", sessionID+".jsonl"))
	return newest(matches)
}

// ParseTranscript implements adapter.Transcripter.
func (c *claude) ParseTranscript(line []byte) []transcript.Entry {
	return parseLine(line)
}

// validSessionID reports whether id is safe to use in a glob pattern: no
// path separators, dots or glob metacharacters.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// newest returns the most recently modified regular file of paths.
func newest(paths []string) (string, bool) {
	var best string
	var bestTime time.Time
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if best == "" || fi.ModTime().After(bestTime) {
			best, bestTime = p, fi.ModTime()
		}
	}
	return best, best != ""
}

// record is the part of a transcript line the chat needs.
type record struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	Timestamp        string `json:"timestamp"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsAPIError       bool   `json:"isApiErrorMessage"`
	Message          struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`

	// system lines
	Content      json.RawMessage `json:"content"`
	Error        json.RawMessage `json:"error"`
	RetryAttempt int             `json:"retryAttempt"`
	MaxRetries   int             `json:"maxRetries"`
}

// block is a content block of a message.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseLine maps one transcript line to chat entries:
//
//	user text, slash command           -> User (images as "[image]")
//	user tool_result                   -> Result (ID = tool_use_id)
//	slash command output,
//	  task notification, interruption  -> Note
//	assistant text                     -> Assistant
//	assistant tool_use                 -> Tool
//	assistant API error                -> Note
//	system compact_boundary, api_error,
//	  informational, local_command     -> Note
//
// Skipped: sidechain (subagent) and meta lines (injected skill text, image
// path notes, caveats), the compaction summary, thinking blocks, synthetic
// replies, and all other line types.
func parseLine(line []byte) []transcript.Entry {
	var r record
	if !decode(line, &r) || r.IsSidechain || r.IsMeta {
		return nil
	}
	var out []transcript.Entry
	switch r.Type {
	case "user":
		if !r.IsCompactSummary {
			out = userContent(r.Message.Content)
		}
	case "assistant":
		out = assistantContent(r)
	case "system":
		out = systemLine(r)
	}
	if ts := timeMs(r.Timestamp); ts != 0 {
		for i := range out {
			out[i].TimeMs = ts
		}
	}
	return out
}

// decode is json.Unmarshal that tolerates fields of an unexpected type
// (they stay zero), so one odd field does not hide a whole line.
func decode(data []byte, v any) bool {
	var te *json.UnmarshalTypeError
	err := json.Unmarshal(data, v)
	return err == nil || errors.As(err, &te)
}

func timeMs(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func note(s string) []transcript.Entry {
	return []transcript.Entry{{Kind: transcript.Note, Text: transcript.Clip(s, transcript.MaxText)}}
}

// userContent maps a user message: a string, or blocks of text, images and
// tool results. Text and images of one message become one User entry.
func userContent(raw json.RawMessage) []transcript.Entry {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return userText(s)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []transcript.Entry
	var parts []string
	flush := func() {
		if len(parts) > 0 {
			out = append(out, transcript.Entry{Kind: transcript.User, Text: transcript.Clip(strings.Join(parts, "\n"), transcript.MaxText)})
			parts = nil
		}
	}
	for _, raw := range blocks {
		var b block
		if !decode(raw, &b) {
			continue
		}
		switch b.Type {
		case "text":
			es := userText(b.Text)
			if len(es) == 1 && es[0].Kind == transcript.User {
				parts = append(parts, es[0].Text)
				continue
			}
			flush()
			out = append(out, es...)
		case "image":
			parts = append(parts, "[image]")
		case "tool_result":
			flush()
			out = append(out, transcript.Entry{Kind: transcript.Result, ID: b.ToolUseID, Output: resultText(b.Content), Error: b.IsError})
		}
	}
	flush()
	return out
}

// userText maps user text. Claude Code records slash commands, their
// output, background task notifications, ! shell commands and
// interruptions as user text wrapped in pseudo-XML tags. A slash command
// is what the user typed ("/review the auth code"), so it stays a User
// entry; the rest become notes. Only a tag at the very start counts, so a
// prompt that merely mentions one stays a prompt.
func userText(s string) []transcript.Entry {
	s = strings.TrimSpace(stripTag(s, "system-reminder"))
	switch {
	case s == "":
		return nil
	case strings.HasPrefix(s, "[Request interrupted by user"):
		return note("interrupted")
	case strings.HasPrefix(s, "<command-name>"), strings.HasPrefix(s, "<command-message>"):
		name, _ := inner(s, "command-name")
		args, _ := inner(s, "command-args")
		if name == "" {
			name, _ = inner(s, "command-message")
		}
		cmd := strings.TrimSpace(strings.TrimSpace(name) + " " + strings.TrimSpace(args))
		if cmd == "" {
			return nil
		}
		return []transcript.Entry{{Kind: transcript.User, Text: transcript.Clip(cmd, transcript.MaxText)}}
	case strings.HasPrefix(s, "<local-command-stdout>"), strings.HasPrefix(s, "<local-command-stderr>"):
		return output(s, "local-command-stdout", "local-command-stderr")
	case strings.HasPrefix(s, "<local-command-caveat>"):
		return nil
	case strings.HasPrefix(s, "<task-notification>"):
		summary, _ := inner(s, "summary")
		if summary = strings.TrimSpace(summary); summary == "" {
			summary = "background task finished"
		}
		return note(summary)
	case strings.HasPrefix(s, "<bash-input>"):
		cmd, _ := inner(s, "bash-input")
		return note("! " + strings.TrimSpace(cmd))
	case strings.HasPrefix(s, "<bash-stdout>"), strings.HasPrefix(s, "<bash-stderr>"):
		return output(s, "bash-stdout", "bash-stderr")
	}
	return []transcript.Entry{{Kind: transcript.User, Text: transcript.Clip(s, transcript.MaxText)}}
}

// output makes a note of the non-empty contents of the given tags.
func output(s string, tags ...string) []transcript.Entry {
	var parts []string
	for _, t := range tags {
		if v, _ := inner(s, t); strings.TrimSpace(v) != "" {
			parts = append(parts, strings.Trim(v, "\r\n"))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []transcript.Entry{{Kind: transcript.Note, Text: transcript.Clip(strings.Join(parts, "\n"), transcript.MaxOutput)}}
}

// inner returns the text between the first <tag> and the </tag> after it.
func inner(s, tag string) (string, bool) {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return "", false
	}
	v, _, ok := strings.Cut(rest, "</"+tag+">")
	return v, ok
}

// stripTag removes every <tag>...</tag> from s.
func stripTag(s, tag string) string {
	open, end := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], end)
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+len(end):]
	}
}

// resultText is the text of a tool result: a string, or text, image and
// tool reference blocks. Claude Code appends system reminders to some
// results (they are for the model) and wraps tool errors in a tag.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var blocks []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ToolName string `json:"tool_name"`
		}
		_ = json.Unmarshal(raw, &blocks)
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "tool_reference":
				parts = append(parts, b.ToolName)
			default:
				parts = append(parts, "["+b.Type+"]")
			}
		}
		s = strings.Join(parts, "\n")
	}
	s = stripTag(s, "system-reminder")
	if strings.HasPrefix(strings.TrimSpace(s), "<tool_use_error>") {
		s, _ = inner(s, "tool_use_error")
	}
	return transcript.Clip(strings.Trim(s, "\r\n"), transcript.MaxOutput)
}

// assistantContent maps an assistant message. API errors are recorded as
// synthetic assistant messages; other synthetic ones ("No response
// requested.") are not the model's.
func assistantContent(r record) []transcript.Entry {
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			return nil
		case r.IsAPIError:
			return note(s)
		case r.Message.Model == "<synthetic>":
			return nil
		}
		return []transcript.Entry{{Kind: transcript.Assistant, Text: transcript.Clip(s, transcript.MaxText)}}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return nil
	}
	var out []transcript.Entry
	for _, raw := range blocks {
		var b block
		if !decode(raw, &b) {
			continue
		}
		switch b.Type {
		case "text":
			text := strings.TrimSpace(b.Text)
			switch {
			case text == "":
			case r.IsAPIError:
				out = append(out, note(text)...)
			case r.Message.Model != "<synthetic>":
				out = append(out, transcript.Entry{Kind: transcript.Assistant, Text: transcript.Clip(text, transcript.MaxText)})
			}
		case "tool_use":
			if b.Name != "" {
				out = append(out, toolUse(b.ID, b.Name, b.Input))
			}
		}
	}
	return out
}

// systemLine maps a system line. Retried API errors are shown because the
// agent looks stuck meanwhile; stop hook summaries, turn durations, recaps
// and remote control status are not.
func systemLine(r record) []transcript.Entry {
	var content string
	_ = json.Unmarshal(r.Content, &content)
	switch r.Subtype {
	case "compact_boundary":
		return note("conversation compacted")
	case "api_error":
		var e struct {
			Formatted string `json:"formatted"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(r.Error, &e)
		msg := e.Formatted
		if msg == "" {
			msg = transcript.OneLine(e.Message)
		}
		if msg == "" {
			msg = "request failed"
		}
		text := "API error: " + msg
		if r.MaxRetries > 0 {
			text += fmt.Sprintf(", retrying (%d/%d)", r.RetryAttempt, r.MaxRetries)
		}
		return note(text)
	case "informational":
		if content = strings.TrimSpace(content); content != "" {
			return note(content)
		}
	case "local_command":
		// A slash command and its output, like the user lines above; a
		// command line without tags (an unknown command) never reached the
		// model, so it is a note too.
		out := userText(content)
		for i := range out {
			out[i].Kind = transcript.Note
		}
		return out
	}
	return nil
}

// toolUse maps a tool call. Text is a one-line summary of its input, Detail
// the input in full: the command, a diff, the file content, or the input
// JSON.
func toolUse(id, name string, input json.RawMessage) transcript.Entry {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	e := transcript.Entry{Kind: transcript.Tool, ID: id, Name: name}

	var text string
	switch name {
	case "Bash":
		text = str("command")
	case "Read", "Write", "Edit", "MultiEdit":
		text = str("file_path")
	case "NotebookEdit", "NotebookRead":
		text = str("notebook_path")
	case "Grep", "Glob":
		text = str("pattern")
		if p := str("path"); text != "" && p != "" {
			text += " in " + p
		}
	case "WebFetch":
		text = str("url")
	case "WebSearch", "ToolSearch":
		text = str("query")
	case "Task", "Agent", "Workflow", "Monitor":
		text = str("description")
	case "Skill":
		text = strings.TrimSpace(str("skill") + " " + str("args"))
	case "TaskCreate":
		text = str("subject")
	case "ExitPlanMode":
		text = str("plan")
	case "TodoWrite":
		if todos, ok := in["todos"].([]any); ok {
			text = plural(len(todos), "todo")
		}
	case "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				text, _ = q["question"].(string)
			}
		}
	}
	if text == "" {
		text = compactJSON(input)
	}
	e.Text = hookcmd.Truncate(text)

	var detail string
	switch name {
	case "Bash":
		detail = str("command")
	case "Edit":
		detail = diff(str("old_string"), str("new_string"))
	case "MultiEdit":
		edits, _ := in["edits"].([]any)
		var hunks []string
		for _, ed := range edits {
			if m, ok := ed.(map[string]any); ok {
				o, _ := m["old_string"].(string)
				n, _ := m["new_string"].(string)
				hunks = append(hunks, "@@\n"+diff(o, n))
			}
		}
		detail = strings.Join(hunks, "\n")
	case "Write":
		detail = str("content")
	case "ExitPlanMode":
		detail = str("plan")
	}
	if detail == "" {
		detail = indentJSON(input)
	}
	if detail = transcript.Clip(detail, transcript.MaxDetail); detail != e.Text {
		e.Detail = detail
	}
	return e
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// diff shows an edit as removed and added lines.
func diff(old, new string) string {
	var b strings.Builder
	for _, l := range lines(old) {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range lines(new) {
		b.WriteString("+" + l + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// compactJSON returns raw on one line, or "" for an empty object.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil || b.String() == "{}" || b.String() == "null" {
		return ""
	}
	return b.String()
}

// indentJSON returns raw indented, or "" for an empty object.
func indentJSON(raw json.RawMessage) string {
	if compactJSON(raw) == "" {
		return ""
	}
	var b bytes.Buffer
	_ = json.Indent(&b, raw, "", "  ")
	return b.String()
}
