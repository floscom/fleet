package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
	"fleet/internal/transcript"
)

// Codex (0.154) writes each session to a "rollout" file,
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local time>-<session id>.jsonl,
// one {timestamp, type, payload} object per line. Subagents get rollout
// files of their own. The conversation as the TUI shows it is in the
// event_msg lines of payload type item_completed, one finished item
// (message, command, file change, ...) each; tool items carry their output.
// The response_item lines hold the same conversation as the model sees it,
// including injected context (AGENTS.md, environment, plugin lists), so they
// are ignored. So are the event_msg types older versions wrote instead of
// item_completed (user_message, agent_message, patch_apply_end, ...): 0.147
// and 0.154 no longer write them, and reading both would show every message
// twice.

var _ adapter.Transcripter = (*codex)(nil)

// TranscriptDir implements adapter.Transcripter. Sessions live in
// CODEX_HOME, like the credentials.
func (c *codex) TranscriptDir(home string) string {
	auth := c.AuthFile(home)
	if auth == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(auth), "sessions")
}

// FindTranscript implements adapter.Transcripter. If several rollouts end
// in the session id, the most recently written wins.
func (c *codex) FindTranscript(dir, sessionID string) (string, bool) {
	if !validSessionID(sessionID) {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
	return newest(matches)
}

// ParseTranscript implements adapter.Transcripter.
func (c *codex) ParseTranscript(line []byte) []transcript.Entry {
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

// rolloutLine is one line of a rollout file.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// event is the payload of an event_msg line.
type event struct {
	Type     string          `json:"type"`
	Reason   string          `json:"reason"`    // turn_aborted
	NumTurns int             `json:"num_turns"` // thread_rolled_back
	Item     json.RawMessage `json:"item"`      // item_completed
}

// parseLine maps one rollout line to chat entries:
//
//	UserMessage       -> User (images as "[image: path]")
//	AgentMessage      -> Assistant
//	CommandExecution  -> Tool "shell" with its output
//	FileChange        -> Tool "edit": the paths, the diffs as Detail
//	McpToolCall       -> Tool "<server>.<tool>" with its result
//	Extension         -> Tool "web_search", "image_gen", "sleep" or its kind
//	ImageView         -> Tool "view_image"
//	ContextCompaction -> Note
//	SubAgentActivity  -> Note when a subagent starts or finishes
//	turn_aborted, thread_rolled_back -> Note
//
// Tool entries have no ID: their output is part of the item, no Result
// follows. Reasoning and collaboration tool calls are skipped.
func parseLine(line []byte) []transcript.Entry {
	var l rolloutLine
	if !decode(line, &l) || l.Type != "event_msg" {
		return nil
	}
	var ev event
	if !decode(l.Payload, &ev) {
		return nil
	}
	var out []transcript.Entry
	switch ev.Type {
	case "item_completed":
		out = itemEntries(ev.Item)
	case "turn_aborted":
		text := "interrupted"
		if ev.Reason != "" && ev.Reason != "interrupted" {
			text = "turn aborted: " + ev.Reason
		}
		out = note(text)
	case "thread_rolled_back":
		out = note(fmt.Sprintf("rolled back %s", plural(max(ev.NumTurns, 1), "turn")))
	}
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		for i := range out {
			out[i].TimeMs = t.UnixMilli()
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

func note(s string) []transcript.Entry {
	return []transcript.Entry{{Kind: transcript.Note, Text: transcript.Clip(s, transcript.MaxText)}}
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// part is an element of a message's content.
type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Path string `json:"path"` // local_image
}

// item is the union of the item_completed item fields fleet reads. Fields
// whose type differs between item types are raw.
type item struct {
	Type string `json:"type"`
	// UserMessage, AgentMessage
	Content []part `json:"content"`
	// CommandExecution
	Command          json.RawMessage `json:"command"` // argv (or a string)
	AggregatedOutput string          `json:"aggregated_output"`
	Stdout           string          `json:"stdout"`
	Stderr           string          `json:"stderr"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	// FileChange
	Changes map[string]fileChange `json:"changes"`
	// McpToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"` // object; a base64 image for image_gen
	Error     json.RawMessage `json:"error"`
	// Extension
	Kind          string          `json:"kind"`
	Query         string          `json:"query"`
	Action        json.RawMessage `json:"action"`
	Results       json.RawMessage `json:"results"`
	DurationMs    int64           `json:"durationMs"`
	RevisedPrompt string          `json:"revisedPrompt"`
	SavedPath     string          `json:"savedPath"`
	Failure       json.RawMessage `json:"failure"`
	// ImageView
	Path string `json:"path"`
	// SubAgentActivity
	AgentPath string `json:"agent_path"`
}

// fileChange is one file of a FileChange item.
type fileChange struct {
	Type        string `json:"type"` // add, update, delete
	Content     string `json:"content"`
	UnifiedDiff string `json:"unified_diff"`
	MovePath    string `json:"move_path"`
}

func itemEntries(raw json.RawMessage) []transcript.Entry {
	var it item
	if !decode(raw, &it) {
		return nil
	}
	switch it.Type {
	case "UserMessage":
		return userMessage(it.Content)
	case "AgentMessage":
		var texts []string
		for _, p := range it.Content {
			if t := strings.TrimSpace(p.Text); t != "" {
				texts = append(texts, t)
			}
		}
		if len(texts) == 0 {
			return nil
		}
		return []transcript.Entry{{Kind: transcript.Assistant, Text: transcript.Clip(strings.Join(texts, "\n\n"), transcript.MaxText)}}
	case "CommandExecution":
		return []transcript.Entry{command(it)}
	case "FileChange":
		return []transcript.Entry{fileChanges(it)}
	case "McpToolCall":
		return []transcript.Entry{mcpCall(it)}
	case "Extension":
		return []transcript.Entry{extension(it)}
	case "ImageView":
		return []transcript.Entry{{Kind: transcript.Tool, Name: "view_image", Text: hookcmd.Truncate(filePath(it.Path))}}
	case "ContextCompaction":
		return note("context compacted")
	case "SubAgentActivity":
		// "completed" follows every turn the subagent was given, not only
		// its first; "interacted" (a message to it) is left out.
		name := strings.TrimSpace("subagent " + it.AgentPath)
		switch it.Kind {
		case "started":
			return note("started " + name)
		case "completed":
			return note(name + " finished")
		}
	}
	return nil
}

// userMessage maps what the user typed. Answers to the agent's questions
// arrive wrapped in a tag as JSON; only the answers are shown.
func userMessage(parts []part) []transcript.Entry {
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			if t := strings.TrimSpace(answers(p.Text)); t != "" {
				texts = append(texts, t)
			}
		case "local_image", "image":
			if p.Path != "" {
				texts = append(texts, "[image: "+p.Path+"]")
			} else {
				texts = append(texts, "[image]")
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return []transcript.Entry{{Kind: transcript.User, Text: transcript.Clip(strings.Join(texts, "\n"), transcript.MaxText)}}
}

// answers turns a question reply into its answers, one per line; other
// text is returned unchanged.
func answers(s string) string {
	const open, end = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, open) || !strings.HasSuffix(t, end) {
		return s
	}
	body := t[len(open) : len(t)-len(end)]
	var as []struct {
		Answer string `json:"answer"`
	}
	if json.Unmarshal([]byte(body), &as) != nil {
		return body
	}
	var out []string
	for _, a := range as {
		out = append(out, a.Answer)
	}
	return strings.Join(out, "\n")
}

// command maps a shell command. Codex runs commands as
// ["/bin/bash", "-lc", "<script>"]; the script is what the model wrote.
func command(it item) transcript.Entry {
	cmd := commandText(it.Command)
	e := transcript.Entry{Kind: transcript.Tool, Name: "shell", Text: hookcmd.Truncate(cmd)}
	if d := transcript.Clip(cmd, transcript.MaxDetail); d != e.Text {
		e.Detail = d
	}
	out := it.AggregatedOutput
	if out == "" {
		out = joinNonEmpty(it.Stdout, it.Stderr)
	}
	e.Output = transcript.Clip(strings.Trim(out, "\r\n"), transcript.MaxOutput)
	e.Error = failed(it.Status) || it.ExitCode != nil && *it.ExitCode != 0
	return e
}

func commandText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil {
		return ""
	}
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		switch path.Base(argv[0]) {
		case "bash", "sh", "zsh", "dash":
			return argv[2]
		}
	}
	for i, a := range argv {
		argv[i] = hookcmd.Quote(a)
	}
	return strings.Join(argv, " ")
}

func failed(status string) bool { return status == "failed" || status == "declined" }

// fileChanges maps an apply_patch: Text lists the files, Detail has a diff
// per file headed by A (added), M (updated) or D (deleted) and its path.
func fileChanges(it item) transcript.Entry {
	paths := make([]string, 0, len(it.Changes))
	for p := range it.Changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var detail strings.Builder
	for _, p := range paths {
		ch := it.Changes[p]
		switch ch.Type {
		case "add":
			fmt.Fprintf(&detail, "A %s\n", p)
			for _, l := range lines(ch.Content) {
				detail.WriteString("+" + l + "\n")
			}
		case "delete":
			fmt.Fprintf(&detail, "D %s\n", p)
		default:
			if ch.MovePath != "" {
				fmt.Fprintf(&detail, "M %s -> %s\n", p, ch.MovePath)
			} else {
				fmt.Fprintf(&detail, "M %s\n", p)
			}
			if ch.UnifiedDiff != "" {
				detail.WriteString(strings.TrimSuffix(ch.UnifiedDiff, "\n") + "\n")
			}
		}
	}
	e := transcript.Entry{
		Kind:   transcript.Tool,
		Name:   "edit",
		Text:   hookcmd.Truncate(strings.Join(paths, ", ")),
		Detail: transcript.Clip(strings.TrimSuffix(detail.String(), "\n"), transcript.MaxDetail),
		Error:  failed(it.Status),
	}
	if e.Error {
		// On success the output only repeats the file list.
		e.Output = transcript.Clip(strings.Trim(joinNonEmpty(it.Stdout, it.Stderr), "\r\n"), transcript.MaxOutput)
	}
	return e
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return strings.TrimRight(a, "\n") + "\n" + b
}

// mcpCall maps an MCP tool call; the result is MCP content blocks.
func mcpCall(it item) transcript.Entry {
	e := transcript.Entry{
		Kind:   transcript.Tool,
		Name:   it.Server + "." + it.Tool,
		Text:   hookcmd.Truncate(compactJSON(it.Arguments)),
		Detail: transcript.Clip(indentJSON(it.Arguments), transcript.MaxDetail),
		Error:  failed(it.Status) || !isNull(it.Error),
	}
	var res struct {
		Content []part `json:"content"`
		IsError bool   `json:"isError"`
	}
	var out string
	if json.Unmarshal(it.Result, &res) == nil && len(res.Content) > 0 {
		var texts []string
		for _, p := range res.Content {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			} else {
				texts = append(texts, "["+p.Type+"]")
			}
		}
		out = strings.Join(texts, "\n")
		e.Error = e.Error || res.IsError
	} else if !isNull(it.Error) {
		out = jsonText(it.Error)
	} else {
		out = compactJSON(it.Result)
	}
	e.Output = transcript.Clip(strings.Trim(out, "\r\n"), transcript.MaxOutput)
	return e
}

// extension maps the built-in tools Codex reports as Extension items.
func extension(it item) transcript.Entry {
	switch it.Kind {
	case "web.search":
		return webSearch(it)
	case "image_gen.generation":
		// The result is the image itself, base64: only the path is shown.
		e := transcript.Entry{
			Kind:   transcript.Tool,
			Name:   "image_gen",
			Text:   hookcmd.Truncate(it.RevisedPrompt),
			Detail: transcript.Clip(it.RevisedPrompt, transcript.MaxDetail),
			Output: it.SavedPath,
			Error:  failed(it.Status) || !isNull(it.Failure),
		}
		if e.Detail == e.Text {
			e.Detail = ""
		}
		if !isNull(it.Failure) {
			e.Output = transcript.Clip(jsonText(it.Failure), transcript.MaxOutput)
		}
		return e
	case "clock.sleep":
		return transcript.Entry{Kind: transcript.Tool, Name: "sleep", Text: (time.Duration(it.DurationMs) * time.Millisecond).String()}
	}
	return transcript.Entry{Kind: transcript.Tool, Name: it.Kind, Error: failed(it.Status)}
}

// webSearch maps a web search: a search, opening a page or finding text in
// it. The results are listed as title and URL.
func webSearch(it item) transcript.Entry {
	var a struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
		Pattern string   `json:"pattern"`
	}
	_ = json.Unmarshal(it.Action, &a)
	text := it.Query
	if text == "" {
		text = joinNonEmpty(a.Query, a.URL)
		if a.Pattern != "" {
			text = strings.TrimSpace(a.Pattern + " in " + a.URL)
		}
	}
	e := transcript.Entry{Kind: transcript.Tool, Name: "web_search", Text: hookcmd.Truncate(text)}
	if len(a.Queries) > 1 {
		e.Detail = transcript.Clip(strings.Join(a.Queries, "\n"), transcript.MaxDetail)
	}
	var results []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal(it.Results, &results)
	var out []string
	for _, r := range results {
		out = append(out, strings.TrimSpace(r.Title+" "+r.URL))
	}
	e.Output = transcript.Clip(strings.Join(out, "\n"), transcript.MaxOutput)
	return e
}

// filePath turns the file:// URLs some items use into paths.
func filePath(s string) string {
	return strings.TrimPrefix(s, "file://")
}

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// jsonText returns a JSON string's value, or other JSON as it is.
func jsonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return compactJSON(raw)
}

// compactJSON returns raw on one line, or "" for null or an empty object.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil || b.String() == "{}" || b.String() == "null" {
		return ""
	}
	return b.String()
}

// indentJSON returns raw indented, or "" for null or an empty object.
func indentJSON(raw json.RawMessage) string {
	if compactJSON(raw) == "" {
		return ""
	}
	var b bytes.Buffer
	_ = json.Indent(&b, raw, "", "  ")
	return b.String()
}
