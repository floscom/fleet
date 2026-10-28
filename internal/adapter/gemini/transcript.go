package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/transcript"
)

// Gemini 0.66.0 appends a session's records to
// <gemini dir>/tmp/<project>/chats/session-<start>-<first 8 of id>.jsonl.
// <project> is a short name per project folder (~/.gemini/projects.json).
// The first line holds the sessionId; then come messages
// ({"id","timestamp","type":"user"|"gemini"|"info",...}) and bookkeeping
// ({"$set"}, {"$patch"}, {"$rewindTo"}). A record whose id was written
// before replaces it: an answer is written once with its text, then again
// each time one of its tool calls finishes, with the calls so far. Tool
// calls are written once done, with their result. The results are repeated
// in a user record of functionResponse parts.

var _ adapter.Transcripter = (*gemini)(nil)

// TranscriptDir implements adapter.Transcripter.
func (g *gemini) TranscriptDir(home string) string {
	dir := geminiDir(home)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "tmp")
}

// FindTranscript implements adapter.Transcripter: the session file whose
// name ends in the start of the id and whose first line names the id.
func (g *gemini) FindTranscript(dir, sessionID string) (string, bool) {
	if !validSessionID(sessionID) || len(sessionID) < 8 {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "chats", "session-*-"+sessionID[:8]+".jsonl"))
	for _, m := range matches {
		if firstSessionID(m) == sessionID {
			return m, true
		}
	}
	return "", false
}

// firstSessionID reads the sessionId of a session file's first line.
func firstSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4096)
	line, _ := r.ReadSlice('\n')
	var head struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(line, &head)
	return head.SessionID
}

// validSessionID reports whether id is safe to use in a glob pattern.
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

// record is a message record of a session file.
type record struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Model     string          `json:"model"`
	ToolCalls []toolCall      `json:"toolCalls"`
}

type toolCall struct {
	Name          string            `json:"name"`
	DisplayName   string            `json:"displayName"`
	Description   string            `json:"description"`
	Status        string            `json:"status"`
	Args          json.RawMessage   `json:"args"`
	Result        []json.RawMessage `json:"result"`
	ResultDisplay json.RawMessage   `json:"resultDisplay"`
}

// part is one part of a message's content.
type part struct {
	Text             *string         `json:"text"`
	FunctionResponse json.RawMessage `json:"functionResponse"`
}

// cancelled is the info record a turn the user interrupted leaves.
const cancelled = "Request cancelled."

// ParseTranscript implements adapter.Transcripter. Lines carry no state
// from one to the next, so a rewritten answer would repeat: its text is
// taken from the record without tool calls, and each rewrite with tool
// calls adds only the last of them, the one that just finished.
func (g *gemini) ParseTranscript(line []byte) []transcript.Entry {
	var r record
	if json.Unmarshal(line, &r) != nil || r.ID == "" {
		return nil
	}
	ts := timeMs(r.Timestamp)
	switch r.Type {
	case "user":
		text := contentText(r.Content)
		switch {
		case text == "", strings.HasPrefix(text, "<session_context>"), strings.HasPrefix(text, "<hook_context>"):
			return nil
		case strings.HasPrefix(text, "/"):
			return []transcript.Entry{{Kind: transcript.Note, Text: transcript.Clip(text, transcript.MaxText), TimeMs: ts}}
		}
		return []transcript.Entry{{Kind: transcript.User, Text: transcript.Clip(text, transcript.MaxText), TimeMs: ts}}
	case "gemini":
		if len(r.ToolCalls) == 0 {
			if text := strings.TrimSpace(contentText(r.Content)); text != "" {
				return []transcript.Entry{{Kind: transcript.Assistant, Text: transcript.Clip(text, transcript.MaxText), TimeMs: ts}}
			}
			return nil
		}
		e := toolEntry(r.ToolCalls[len(r.ToolCalls)-1])
		e.TimeMs = ts
		return []transcript.Entry{e}
	case "info", "error", "warning":
		text := strings.TrimSpace(contentText(r.Content))
		if text == cancelled {
			text = transcript.Interrupted
		}
		if text == "" {
			return nil
		}
		return []transcript.Entry{{Kind: transcript.Note, Text: transcript.Clip(text, transcript.MaxText), TimeMs: ts}}
	}
	return nil
}

// contentText is the text of a message's content: a string, or the text of
// its parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []part
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Text != nil && len(p.FunctionResponse) == 0 {
			texts = append(texts, *p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// toolEntry is the chat entry of a finished tool call.
func toolEntry(c toolCall) transcript.Entry {
	var args map[string]any
	_ = json.Unmarshal(c.Args, &args)
	str := func(k string) string { s, _ := args[k].(string); return s }
	e := transcript.Entry{Kind: transcript.Tool, Name: c.DisplayName, Error: c.Status != "" && c.Status != "success"}
	if e.Name == "" {
		e.Name = c.Name
	}
	e.Text = firstLine(c.Description)
	if e.Text == "" {
		e.Text = firstLine(str("command"))
	}
	if f := str("file_path"); f != "" && (c.Name == "write_file" || c.Name == "replace") {
		e.Files = []string{f}
	}

	var display any
	_ = json.Unmarshal(c.ResultDisplay, &display)
	switch d := display.(type) {
	case string:
		e.Output = d
	case []any:
		e.Output = snapshotText(d)
	case map[string]any:
		if diff, ok := d["fileDiff"].(string); ok {
			e.Detail = diff
		}
	}
	if e.Detail == "" {
		if cmd := str("command"); cmd != "" {
			e.Detail = cmd
		} else if len(c.Args) > 0 && string(c.Args) != "{}" {
			var b bytes.Buffer
			if json.Indent(&b, c.Args, "", "  ") == nil {
				e.Detail = b.String()
			}
		}
	}
	if e.Output == "" {
		e.Output = responseText(c.Result)
	}
	e.Detail = transcript.Clip(e.Detail, transcript.MaxDetail)
	e.Output = transcript.Clip(strings.TrimSpace(e.Output), transcript.MaxOutput)
	return e
}

// snapshotText is the text of a terminal snapshot, how a shell command's
// result is shown: lines of runs of styled text.
func snapshotText(lines []any) string {
	var out []string
	for _, l := range lines {
		runs, _ := l.([]any)
		var b strings.Builder
		for _, r := range runs {
			if m, ok := r.(map[string]any); ok {
				t, _ := m["text"].(string)
				b.WriteString(t)
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// responseText is what a tool call returned to the model: the output or
// error of its functionResponse parts, without the wrapper that marks
// command output as untrusted and the line naming the command's process
// group.
func responseText(result []json.RawMessage) string {
	var out []string
	for _, raw := range result {
		var p struct {
			FunctionResponse struct {
				Response struct {
					Output string `json:"output"`
					Error  string `json:"error"`
				} `json:"response"`
			} `json:"functionResponse"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		r := p.FunctionResponse.Response
		for _, s := range []string{r.Output, r.Error} {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	var lines []string
	for _, l := range strings.Split(strings.Join(out, "\n"), "\n") {
		if l == "<untrusted_context>" || l == "</untrusted_context>" || strings.HasPrefix(l, "Process Group PGID:") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func timeMs(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
