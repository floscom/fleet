package claude

// Claude Code's Workflow tool runs a script that starts subagents in the
// background of a session. Claude Code 2.1.280 records each run next to the
// session's transcript (<session> is its path without ".jsonl"):
//
//	<session>/workflows/scripts/<name>-<run id>.js
//	    the script; `export const meta = {name, description, phases}` at
//	    its top names the run and its phases (see scriptMeta)
//	<session>/subagents/workflows/<run id>/journal.jsonl
//	    a line per agent started ({"type":"started","agentId","label",
//	    "phase"}), finished ("result", with its return value) or failed
//	    ("failed"), without times
//	<session>/subagents/workflows/<run id>/agent-<agent id>.jsonl
//	    each agent's transcript: the session transcript's format, every
//	    line a sidechain
//
// How a run ended is only in the session transcript: the Workflow call's
// result names the run and its background task ("toolUseResult": {"taskId",
// "runId"}), and when the run ends a <task-notification> for that task
// carries its status, summary and result (queued, then delivered as an
// attachment or a user message: the same text up to three times). The
// result is cut at 8000 characters there; the whole of it, and the lines
// the script logged, are in the task's output file, written as the run
// ends (see taskOutput).
//
// Transcripts grow by megabytes while agents work. workflowCache keeps what
// was read of each file, so following a run reads only what was appended.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/adapter/hookcmd"
	"fleet/internal/transcript"
	"fleet/internal/workflow"
)

var _ adapter.Workflower = (*claude)(nil)

// maxResult caps a run's result.
const maxResult = 64 << 10

// Workflows implements adapter.Workflower.
func (c *claude) Workflows(path string) []workflow.Run {
	session := strings.TrimSuffix(path, ".jsonl")
	runsDir := filepath.Join(session, "subagents", "workflows")
	dirs := subdirs(runsDir)
	subs := agentFiles(session)
	if len(dirs) == 0 && len(subs) == 0 {
		return nil
	}
	c.wf.sweep()
	var ends sessionScan
	c.wf.follow(path, func() lineReader { return &sessionScan{} }, func(r lineReader) {
		ends = r.(*sessionScan).snapshot()
	})
	// A script is named after the run it was written for; a run of a
	// script written before (Workflow's scriptPath) names it in its launch.
	scripts := map[string]string{} // run id -> script
	for _, e := range readDir(filepath.Join(session, "workflows", "scripts")) {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".js") {
			continue
		}
		if i := strings.LastIndex(name, "-wf_"); i >= 0 {
			scripts[strings.TrimSuffix(name[i+1:], ".js")] = filepath.Join(session, "workflows", "scripts", name)
		}
	}
	projects := filepath.Dir(filepath.Dir(path))
	var runs []workflow.Run
	for _, id := range dirs {
		if !validID(id) {
			continue
		}
		script := scripts[id]
		if l := ends.launches[id]; l.script != "" && strings.HasSuffix(l.script, ".js") && within(projects, l.script) {
			script = l.script
		}
		r := c.run(filepath.Join(runsDir, id), id, script, ends)
		if l, ok := ends.launches[id]; ok && r.Status != workflow.Running {
			if out := ends.ends[l.task].output; taskOutputFile(out, filepath.Base(session), l.task) {
				if result, logs, ok := c.wf.taskOutput(out); ok {
					r.Result, r.Logs = result, logs
				}
			}
		}
		runs = append(runs, r)
	}
	runs = append(runs, c.agentRuns(session, subs, ends)...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedMs < runs[j].StartedMs })
	return runs
}

// WorkflowTranscript implements adapter.Workflower.
func (c *claude) WorkflowTranscript(path, run, agent string) (string, transcript.Parser, bool) {
	if !validID(run) || !validID(agent) {
		return "", nil, false
	}
	if strings.HasPrefix(run, agentRunPrefix) {
		p, ok := agentTranscript(strings.TrimSuffix(path, ".jsonl"), agent)
		return p, parseSidechain, ok
	}
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "workflows", run)
	p := filepath.Join(dir, "agent-"+agent+".jsonl")
	if !noLinks(filepath.Dir(dir), dir, p) {
		return "", nil, false
	}
	return p, parseSidechain, true
}

// run reads one run from its directory.
func (c *claude) run(dir, id, script string, ends sessionScan) workflow.Run {
	r := workflow.Run{ID: id, Status: workflow.Running, Phases: []workflow.Phase{}, Agents: []workflow.Agent{}}
	if script != "" {
		r.Name, r.Description, r.Phases = c.wf.meta(script)
		if fi, err := os.Lstat(script); err == nil {
			r.StartedMs = fi.ModTime().UnixMilli()
		}
	}
	if l, ok := ends.launches[id]; ok {
		r.StartedMs = l.ms
		if r.Name == "" {
			r.Name, r.Description = l.name, l.summary
		}
		if e, ok := ends.ends[l.task]; ok {
			r.Status, r.Summary, r.Result = e.status, e.summary, e.result
			r.EndedMs = max(e.ms, r.StartedMs) // a quick failure is queued before the launch is written
		}
	}
	if r.Name == "" {
		r.Name = id
	}

	var agents []journalAgent
	c.wf.follow(filepath.Join(dir, "journal.jsonl"), func() lineReader { return &journalScan{} }, func(r lineReader) {
		agents = r.(*journalScan).snapshot()
	})
	for _, ja := range agents {
		a := workflow.Agent{ID: ja.id, Label: ja.label, Phase: ja.phase, Status: ja.status}
		if a.Label == "" && a.Phase == "" {
			a.Label, a.Phase = c.wf.agentMeta(filepath.Join(dir, "agent-"+ja.id+".meta.json"))
		}
		c.wf.follow(filepath.Join(dir, "agent-"+ja.id+".jsonl"), func() lineReader { return &agentScan{} }, func(r lineReader) {
			r.(*agentScan).fill(&a)
		})
		r.Agents = append(r.Agents, a)
	}
	if r.StartedMs == 0 && len(r.Agents) > 0 {
		r.StartedMs = r.Agents[0].StartedMs
	}
	r.Tally()
	return r
}

// validID reports whether id is safe as a file name part: letters,
// digits, - and _.
func validID(id string) bool {
	return validSessionID(id)
}

// subdirs lists the directories in dir (not symlinks to one).
func subdirs(dir string) []string {
	var out []string
	for _, e := range readDir(dir) {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// readDir lists dir, unless it is a symlink: a sandboxed agent's home is
// the agent's to change, and must not point the daemon at other files.
func readDir(dir string) []os.DirEntry {
	if !noLinks(dir) {
		return nil
	}
	es, _ := os.ReadDir(dir)
	return es
}

// within reports whether path lies below dir.
func within(dir, path string) bool {
	return filepath.IsAbs(path) && strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// noLinks reports whether every path exists and none is a symlink.
func noLinks(paths ...string) bool {
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- cache

// cacheIdle is how long the cache keeps what it read of a file nobody asked
// about.
const cacheIdle = 15 * time.Minute

// workflowCache remembers how far each file was read and what was found.
type workflowCache struct {
	mu      sync.Mutex
	files   map[string]*tail
	metas   map[string]scriptInfo
	outputs map[string]outputInfo
}

// lineReader takes the complete lines of a file, in order.
type lineReader interface{ add(line []byte) }

// tail is how far a file was read, into which reader. Its lock is held
// while reading and using it; the cache's lock guards used.
type tail struct {
	mu   sync.Mutex
	off  int64
	used time.Time
	r    lineReader
}

type scriptInfo struct {
	size              int64
	name, description string
	phases            []workflow.Phase
	used              time.Time
}

// follow reads the complete lines appended to the regular file at path
// since the last call into its reader (made by fresh the first time, and
// again when the file shrank: it was replaced), then hands the reader to
// use. It reports false, without calling use, if there is no such file.
func (c *workflowCache) follow(path string, fresh func() lineReader, use func(lineReader)) bool {
	fi, err := os.Lstat(path)
	c.mu.Lock()
	if err != nil || !fi.Mode().IsRegular() {
		delete(c.files, path)
		c.mu.Unlock()
		return false
	}
	t := c.files[path]
	if t == nil {
		if c.files == nil {
			c.files = map[string]*tail{}
		}
		t = &tail{r: fresh()}
		c.files[path] = t
	}
	t.used = time.Now()
	c.mu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if fi.Size() < t.off {
		t.off, t.r = 0, fresh()
	}
	add := func(line []byte) []transcript.Entry {
		t.r.add(line)
		return nil
	}
	for t.off < fi.Size() {
		p, err := transcript.Forward(path, t.off, add)
		if err != nil || p.End == t.off {
			break // unreadable, or only a line still being written
		}
		t.off = p.End
		if !p.More {
			break
		}
	}
	use(t.r)
	return true
}

// sweep forgets files nobody asked about for cacheIdle.
func (c *workflowCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, t := range c.files {
		if time.Since(t.used) > cacheIdle {
			delete(c.files, p)
		}
	}
	for p, m := range c.metas {
		if time.Since(m.used) > cacheIdle {
			delete(c.metas, p)
		}
	}
	for p, o := range c.outputs {
		if time.Since(o.used) > cacheIdle {
			delete(c.outputs, p)
		}
	}
}

// meta reads a script's meta, again only when its size changed.
func (c *workflowCache) meta(path string) (name, description string, phases []workflow.Phase) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxScript {
		return "", "", []workflow.Phase{}
	}
	c.mu.Lock()
	m, ok := c.metas[path]
	c.mu.Unlock()
	if !ok || m.size != fi.Size() {
		src, err := os.ReadFile(path)
		if err != nil {
			return "", "", []workflow.Phase{}
		}
		m = scriptInfo{size: fi.Size()}
		m.name, m.description, m.phases = scriptMeta(src)
	}
	m.used = time.Now()
	c.mu.Lock()
	if c.metas == nil {
		c.metas = map[string]scriptInfo{}
	}
	c.metas[path] = m
	c.mu.Unlock()
	return m.name, m.description, append([]workflow.Phase{}, m.phases...)
}

// maxOutput caps the size of a task output file read.
const maxOutput = 8 << 20

type outputInfo struct {
	size   int64
	result string
	logs   []string
	used   time.Time
}

// taskOutputFile reports whether path is where Claude Code keeps the output
// of task task of session session: <its temp dir>/<project>/<session>/
// tasks/<task>.output. The path comes from the transcript, which a
// sandboxed agent can write: nothing else is read.
func taskOutputFile(path, session, task string) bool {
	tmp := filepath.Join(os.TempDir(), "claude-"+strconv.Itoa(os.Getuid()))
	path = filepath.Clean(path)
	tasks := filepath.Dir(path)
	return validID(task) && validID(session) && within(tmp, path) &&
		filepath.Base(path) == task+".output" && filepath.Base(tasks) == "tasks" &&
		filepath.Base(filepath.Dir(tasks)) == session
}

// taskOutput reads a workflow task's output file (JSON: summary, logs,
// result, ...), again only when its size changed. It is written when the
// run ends; ok is false before, and for a run that failed without a result.
func (c *workflowCache) taskOutput(path string) (result string, logs []string, ok bool) {
	if !noLinks(path) {
		return "", nil, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 || fi.Size() > maxOutput {
		return "", nil, false
	}
	c.mu.Lock()
	o, cached := c.outputs[path]
	c.mu.Unlock()
	if !cached || o.size != fi.Size() {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, false
		}
		var out struct {
			Logs   []string        `json:"logs"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(data, &out) != nil || len(out.Result) == 0 {
			return "", nil, false
		}
		o = outputInfo{size: fi.Size(), logs: out.Logs}
		if json.Unmarshal(out.Result, &o.result) != nil { // not a string: the JSON
			o.result = string(out.Result)
		}
		o.result = transcript.Clip(o.result, maxResult)
		if len(o.logs) > maxLogs {
			o.logs = append(o.logs[:maxLogs:maxLogs], fmt.Sprintf("… (%d more lines)", len(out.Logs)-maxLogs))
		}
		for i, l := range o.logs {
			o.logs[i] = transcript.Clip(l, transcript.MaxDetail)
		}
	}
	o.used = time.Now()
	c.mu.Lock()
	if c.outputs == nil {
		c.outputs = map[string]outputInfo{}
	}
	c.outputs[path] = o
	c.mu.Unlock()
	return o.result, append([]string(nil), o.logs...), true
}

// maxLogs caps the log lines of a run.
const maxLogs = 500

// agentMeta reads the label and phase from an agent's meta file, for
// journals that did not record them.
func (c *workflowCache) agentMeta(path string) (label, phase string) {
	if !noLinks(path) {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 {
		return "", ""
	}
	var m struct {
		Description   string `json:"description"`
		WorkflowPhase string `json:"workflowPhase"`
	}
	_ = json.Unmarshal(data, &m)
	return m.Description, m.WorkflowPhase
}

// ------------------------------------------------------ session transcript

// sessionScan finds the runs a session launched and how they ended.
type sessionScan struct {
	launches map[string]launch // run id -> its latest launch
	ends     map[string]ending // task id -> how it ended
	// agentEnds are the latest notifications of tasks: a subagent's task
	// notifies each time it stops.
	agentEnds map[string]ending
}

type launch struct {
	task, name, summary, script string
	ms                          int64
}

type ending struct {
	status, summary, result string
	// output is the task's output file, as the notification names it.
	output string
	ms     int64
}

var (
	launchMark = []byte(`"local_workflow"`)
	noteMark   = []byte(`<task-notification>`)
	// noteMarkEscaped is noteMark as JSON encoders that escape HTML write
	// it (Claude Code does not).
	noteMarkEscaped = []byte(`\u003ctask-notification\u003e`)
)

func (s *sessionScan) add(line []byte) {
	switch {
	case bytes.Contains(line, noteMark) || bytes.Contains(line, noteMarkEscaped):
		// Queued, then delivered: a queue-operation's content, an
		// attachment's prompt, a user message's text. Anything else that
		// holds one quotes it (a command, a file read), and is no note.
		var l struct {
			Type       string          `json:"type"`
			Timestamp  string          `json:"timestamp"`
			Content    json.RawMessage `json:"content"`
			Attachment struct {
				Prompt json.RawMessage `json:"prompt"`
			} `json:"attachment"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if !decode(line, &l) {
			return
		}
		var texts []string
		switch l.Type {
		case "queue-operation":
			texts = stringsOf(l.Content)
		case "attachment":
			texts = stringsOf(l.Attachment.Prompt)
		case "user":
			texts = stringsOf(l.Message.Content)
		}
		for _, text := range texts {
			if strings.HasPrefix(strings.TrimSpace(text), "<task-notification>") {
				s.note(text, timeMs(l.Timestamp))
			}
		}
	case bytes.Contains(line, launchMark):
		var l struct {
			Timestamp     string          `json:"timestamp"`
			ToolUseResult json.RawMessage `json:"toolUseResult"`
		}
		if json.Unmarshal(line, &l) != nil {
			return
		}
		var r struct {
			TaskID       string `json:"taskId"`
			TaskType     string `json:"taskType"`
			RunID        string `json:"runId"`
			WorkflowName string `json:"workflowName"`
			Summary      string `json:"summary"`
			ScriptPath   string `json:"scriptPath"`
		}
		if json.Unmarshal(l.ToolUseResult, &r) != nil || r.TaskType != "local_workflow" || r.RunID == "" {
			return
		}
		if s.launches == nil {
			s.launches = map[string]launch{}
		}
		s.launches[r.RunID] = launch{task: r.TaskID, name: r.WorkflowName, summary: r.Summary,
			script: filepath.Clean(r.ScriptPath), ms: timeMs(l.Timestamp)}
	}
}

// note records a task notification: the first one of a task counts.
func (s *sessionScan) note(text string, ms int64) {
	task, _ := inner(text, "task-id")
	status, _ := inner(text, "status")
	if task = strings.TrimSpace(task); task == "" {
		return
	}
	summary, _ := inner(text, "summary")
	result, _ := inner(text, "result")
	output, _ := inner(text, "output-file")
	e := ending{
		summary: strings.TrimSpace(html.UnescapeString(summary)),
		result:  transcript.Clip(strings.TrimSpace(html.UnescapeString(result)), maxResult),
		output:  strings.TrimSpace(html.UnescapeString(output)),
		ms:      ms,
	}
	switch strings.TrimSpace(status) {
	case "completed":
		e.status = workflow.Completed
	case "failed":
		e.status = workflow.Failed
	default:
		e.status = workflow.Stopped
	}
	if s.agentEnds == nil {
		s.agentEnds = map[string]ending{}
	}
	if latest, ok := s.agentEnds[task]; !ok || e.ms >= latest.ms {
		s.agentEnds[task] = e
	}
	if s.ends[task].status != "" {
		return
	}
	if s.ends == nil {
		s.ends = map[string]ending{}
	}
	s.ends[task] = e
}

func (s *sessionScan) snapshot() sessionScan {
	return sessionScan{launches: clone(s.launches), ends: clone(s.ends), agentEnds: clone(s.agentEnds)}
}

func clone[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stringsOf returns the text of message content: a string, or the text
// blocks of a list.
func stringsOf(raw json.RawMessage) []string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []string{str}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// ------------------------------------------------------------- journal

// journalScan collects a run's agents from its journal.
type journalScan struct {
	agents []journalAgent
	index  map[string]int
}

type journalAgent struct {
	id, label, phase, status string
}

func (j *journalScan) add(line []byte) {
	var e struct {
		Type    string `json:"type"`
		AgentID string `json:"agentId"`
		Label   string `json:"label"`
		Phase   string `json:"phase"`
	}
	if !decode(line, &e) || !validID(e.AgentID) {
		return
	}
	if j.index == nil {
		j.index = map[string]int{}
	}
	i, ok := j.index[e.AgentID]
	if !ok {
		i = len(j.agents)
		j.index[e.AgentID] = i
		j.agents = append(j.agents, journalAgent{id: e.AgentID, status: workflow.Running})
	}
	a := &j.agents[i]
	if e.Label != "" {
		a.label = e.Label
	}
	if e.Phase != "" {
		a.phase = e.Phase
	}
	switch e.Type {
	case "started":
		a.status = workflow.Running
	case "result":
		a.status = workflow.Done
	case "failed":
		a.status = workflow.Failed
	}
}

func (j *journalScan) snapshot() []journalAgent {
	return append([]journalAgent(nil), j.agents...)
}

// --------------------------------------------------------- agent transcript

// agentScan follows one agent's transcript.
type agentScan struct {
	firstMs, lastMs int64
	model           string
	tokens          int64
	toolUses        int
	tool, activity  string
	lastCall        string // id of the latest tool call counted
	// ended: its latest message ended its turn.
	ended bool
}

var (
	assistantMark = []byte(`"type":"assistant"`)
	timeMark      = []byte(`"timestamp":"`)
)

func (s *agentScan) add(line []byte) {
	if !bytes.Contains(line, assistantMark) {
		// Tool results and bookkeeping: only their time counts, and
		// decoding them all is most of the work.
		if i := bytes.LastIndex(line, timeMark); i >= 0 {
			rest := line[i+len(timeMark):]
			if j := bytes.IndexByte(rest, '"'); j > 0 {
				s.at(timeMs(string(rest[:j])))
			}
		}
		return
	}
	var r struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Model string `json:"model"`
			Usage struct {
				Input         int64 `json:"input_tokens"`
				Output        int64 `json:"output_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if !decode(line, &r) {
		return
	}
	s.at(timeMs(r.Timestamp))
	if r.Type != "assistant" {
		return
	}
	m := r.Message
	// A message is written a line per content block, all but the last
	// without a stop reason.
	s.ended = m.StopReason != "" && m.StopReason != "tool_use" && m.StopReason != "pause_turn"
	if m.Model != "" && m.Model != "<synthetic>" {
		s.model = m.Model
	}
	// Every content block of a message is a line of its own, each with
	// the message's usage so far.
	if u := m.Usage; u.Input+u.Output+u.CacheRead+u.CacheCreation > 0 {
		s.tokens = u.Input + u.Output + u.CacheRead + u.CacheCreation
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			if b.ID != "" && b.ID == s.lastCall {
				continue
			}
			s.lastCall = b.ID
			s.toolUses++
			if b.Name == "StructuredOutput" {
				// How an agent asked for a schema returns: its result.
				s.tool, s.activity = "", structuredSummary(b.Input)
				continue
			}
			s.tool, s.activity = b.Name, toolUse(b.ID, b.Name, b.Input).Text
		case "text":
			if t := transcript.OneLine(b.Text); t != "" {
				s.tool, s.activity = "", hookcmd.Truncate(t)
			}
		}
	}
}

// structuredSummary sums up a structured result: its summary (or first)
// text field, else its JSON.
func structuredSummary(input json.RawMessage) string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(input, &m)
	var first string
	for _, k := range []string{"summary", "result", "answer"} {
		if json.Unmarshal(m[k], &first) == nil && strings.TrimSpace(first) != "" {
			return hookcmd.Truncate(transcript.OneLine(first))
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var s string
		if json.Unmarshal(m[k], &s) == nil && strings.TrimSpace(s) != "" {
			return hookcmd.Truncate(transcript.OneLine(s))
		}
	}
	return hookcmd.Truncate(compactJSON(input))
}

// at notes a line written at ms.
func (s *agentScan) at(ms int64) {
	if ms == 0 {
		return
	}
	if s.firstMs == 0 {
		s.firstMs = ms
	}
	s.lastMs = max(s.lastMs, ms)
}

// fill copies what the scan found into a.
func (s *agentScan) fill(a *workflow.Agent) {
	a.StartedMs, a.UpdatedMs = s.firstMs, s.lastMs
	a.Model, a.Tokens, a.ToolUses = s.model, s.tokens, s.toolUses
	a.Tool, a.Activity = s.tool, s.activity
}

// ------------------------------------------------------------ script meta

// maxScript caps the size of a script read for its meta.
const maxScript = 1 << 20

var metaStart = regexp.MustCompile(`export\s+const\s+meta\s*=\s*`)

// scriptMeta reads `export const meta = {...}` from a workflow script: a
// plain JavaScript object literal (the Workflow tool demands one).
func scriptMeta(src []byte) (name, description string, phases []workflow.Phase) {
	phases = []workflow.Phase{}
	loc := metaStart.FindIndex(src)
	if loc == nil {
		return
	}
	p := &jsParser{src: src, pos: loc[1]}
	v, ok := p.value()
	m, _ := v.(map[string]any)
	if !ok || m == nil {
		return
	}
	name, _ = m["name"].(string)
	description, _ = m["description"].(string)
	list, _ := m["phases"].([]any)
	for _, x := range list {
		ph, _ := x.(map[string]any)
		title, _ := ph["title"].(string)
		if title == "" {
			continue
		}
		detail, _ := ph["detail"].(string)
		phases = append(phases, workflow.Phase{Title: title, Detail: detail})
	}
	return
}

// jsParser reads a JavaScript literal: objects, arrays, strings (quoted or
// template literals without substitutions), numbers, true, false, null.
type jsParser struct {
	src   []byte
	pos   int
	depth int
}

func (p *jsParser) value() (any, bool) {
	p.space()
	if p.pos >= len(p.src) || p.depth > 32 {
		return nil, false
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '\'' || c == '"' || c == '`':
		return p.str()
	default:
		start := p.pos
		for p.pos < len(p.src) && (isIdent(p.src[p.pos]) || strings.IndexByte("+-.", p.src[p.pos]) >= 0) {
			p.pos++
		}
		switch word := string(p.src[start:p.pos]); word {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null", "undefined":
			return nil, true
		case "":
			return nil, false
		default:
			return word, true // a number: kept as its text
		}
	}
}

func (p *jsParser) object() (any, bool) {
	p.pos++ // {
	p.depth++
	defer func() { p.depth-- }()
	m := map[string]any{}
	for {
		p.space()
		if p.pos >= len(p.src) {
			return nil, false
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return m, true
		}
		var key string
		if c := p.src[p.pos]; c == '\'' || c == '"' || c == '`' {
			k, ok := p.str()
			if !ok {
				return nil, false
			}
			key = k.(string)
		} else {
			start := p.pos
			for p.pos < len(p.src) && isIdent(p.src[p.pos]) {
				p.pos++
			}
			if key = string(p.src[start:p.pos]); key == "" {
				return nil, false
			}
		}
		p.space()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, false
		}
		p.pos++
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		m[key] = v
		if !p.comma('}') {
			return nil, false
		}
	}
}

func (p *jsParser) array() (any, bool) {
	p.pos++ // [
	p.depth++
	defer func() { p.depth-- }()
	list := []any{}
	for {
		p.space()
		if p.pos >= len(p.src) {
			return nil, false
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return list, true
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		list = append(list, v)
		if !p.comma(']') {
			return nil, false
		}
	}
}

// comma skips the comma after a member, if any; without one, end must follow.
func (p *jsParser) comma(end byte) bool {
	p.space()
	if p.pos < len(p.src) && p.src[p.pos] == ',' {
		p.pos++
		return true
	}
	return p.pos < len(p.src) && p.src[p.pos] == end
}

func (p *jsParser) str() (any, bool) {
	q := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		p.pos++
		switch {
		case c == q:
			return b.String(), true
		case c == '\\' && p.pos < len(p.src):
			e := p.src[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\n':
				// a line continuation
			case 'u':
				if p.pos+4 <= len(p.src) {
					if r, err := strconv.ParseUint(string(p.src[p.pos:p.pos+4]), 16, 32); err == nil {
						b.WriteRune(rune(r))
						p.pos += 4
						continue
					}
				}
				b.WriteByte(e)
			default:
				b.WriteByte(e)
			}
		case q == '`' && c == '$' && p.pos < len(p.src) && p.src[p.pos] == '{':
			return nil, false // a substitution: not a literal
		case c == '\n' && q != '`':
			return nil, false
		default:
			b.WriteByte(c)
		}
	}
	return nil, false
}

// space skips white space and comments.
func (p *jsParser) space() {
	for p.pos < len(p.src) {
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '*':
			end := bytes.Index(p.src[p.pos+2:], []byte("*/"))
			if end < 0 {
				p.pos = len(p.src)
				return
			}
			p.pos += end + 4
		default:
			return
		}
	}
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
