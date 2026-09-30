package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"fleet/internal/adapter"
	"fleet/internal/transcript"
	"fleet/internal/workflow"
)

// wfSession lays out a session as Claude Code 2.1.280 records workflow
// runs (see workflow.go), in a temp dir.
type wfSession struct {
	t          *testing.T
	transcript string // <projects>/p/s1.jsonl
	session    string // <projects>/p/s1
	tmp        string // TMPDIR, for task output files
}

func newWFSession(t *testing.T) *wfSession {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	t.Setenv("TMPDIR", tmp)
	s := &wfSession{t: t, transcript: filepath.Join(root, "projects", "p", "s1.jsonl"),
		session: filepath.Join(root, "projects", "p", "s1"), tmp: tmp}
	s.write(s.transcript, "")
	return s
}

func (s *wfSession) write(path, content string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *wfSession) append(path string, lines ...string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func (s *wfSession) runDir(run string) string {
	return filepath.Join(s.session, "subagents", "workflows", run)
}

func (s *wfSession) script(name, run string) string {
	return filepath.Join(s.session, "workflows", "scripts", name+"-"+run+".js")
}

// outputFile is where the output of task task goes.
func (s *wfSession) outputFile(task string) string {
	return filepath.Join(s.tmp, "claude-"+strconv.Itoa(os.Getuid()), "p", "s1", "tasks", task+".output")
}

func launchLine(run, task, name, summary, script string) string {
	r, _ := json.Marshal(map[string]string{"status": "async_launched", "taskId": task, "taskType": "local_workflow",
		"workflowName": name, "runId": run, "summary": summary, "scriptPath": script})
	return `{"type":"user",` + stamp + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Workflow launched"}]},"toolUseResult":` + string(r) + `}`
}

func notification(task, status, summary, result, output string) string {
	return "<task-notification>\n<task-id>" + task + "</task-id>\n<tool-use-id>toolu_1</tool-use-id>\n<output-file>" + output +
		"</output-file>\n<status>" + status + "</status>\n<summary>" + summary + "</summary>\n<result>" + result + "</result>\n</task-notification>"
}

// queued is the line of a queued notification. JSON as Claude Code writes
// it: without \u003c for <.
func queued(text string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(text)
	return `{"type":"queue-operation","operation":"enqueue",` + stamp + `,"content":` + strings.TrimSpace(b.String()) + `}`
}

// sideLine is a line of a workflow agent's transcript.
func sideLine(typ, msg string) string {
	return `{"parentUuid":"p","isSidechain":true,"agentId":"a1",` + stamp + `,"type":"` + typ + `","message":` + msg + `}`
}

func sideAssistant(id, content string, in, out int) string {
	return sideLine("assistant", `{"id":"`+id+`","model":"claude-opus-5-5","role":"assistant","content":`+content+
		`,"usage":{"input_tokens":`+strconv.Itoa(in)+`,"output_tokens":`+strconv.Itoa(out)+`,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0}}`)
}

const demoScript = `export const meta = {
  name: 'demo',
  // a comment
  description: "Read, then \"write\", then check",
  phases: [
    { title: 'Read', detail: ` + "`" + `two readers` + "`" + ` },
    { title: 'Write' }, /* trailing comma next */
    { title: 'Check', detail: 'one‑checker', },
  ],
}

phase('Read')
const x = await agent(` + "`${GOAL}`" + `)
`

func runsByID(runs []workflow.Run) map[string]workflow.Run {
	out := map[string]workflow.Run{}
	for _, r := range runs {
		out[r.ID] = r
	}
	return out
}

func TestWorkflows(t *testing.T) {
	s := newWFSession(t)
	c := &claude{}
	if runs := c.Workflows(s.transcript); runs != nil {
		t.Fatalf("a session without runs: %+v", runs)
	}

	script := s.script("demo", "wf_1")
	s.write(script, demoScript)
	s.append(s.transcript, launchLine("wf_1", "wt1", "demo", "Demo run", script))
	dir := s.runDir("wf_1")
	s.append(filepath.Join(dir, "journal.jsonl"),
		`{"type":"launched"}`,
		`{"type":"started","key":"v2:1","agentId":"a1","label":"read:x","phase":"Read"}`,
		`{"type":"started","key":"v2:2","agentId":"a2","label":"read:y","phase":"Read"}`,
		`{"type":"result","key":"v2:1","agentId":"a1","result":"# Map of x\nmore"}`,
		`{"type":"started","key":"v2:3","agentId":"a3"}`)
	s.write(filepath.Join(dir, "agent-a3.meta.json"), `{"agentType":"workflow-subagent","description":"write","workflowPhase":"Write"}`)
	s.append(filepath.Join(dir, "agent-a1.jsonl"),
		sideLine("user", `{"role":"user","content":"[Workflow harness — computed task] The task text below was computed at runtime. The computed task text follows:\n  Map x.\n  In detail."}`),
		// Every block of a message is a line, each with the usage so far.
		sideAssistant("m1", `[{"type":"thinking","thinking":"hm"}]`, 2, 8),
		sideAssistant("m1", `[{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"ls -la"}}]`, 2, 40),
		sideLine("user", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"total 0"}]}`),
		sideAssistant("m2", `[{"type":"text","text":"# Map of x\nmore"}]`, 3, 500))
	s.append(filepath.Join(dir, "agent-a2.jsonl"),
		sideAssistant("m1", `[{"type":"tool_use","id":"toolu_b","name":"Read","input":{"file_path":"/w/y.go"}}]`, 2, 30))

	runs := c.Workflows(s.transcript)
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	r := runs[0]
	wantPhases := []workflow.Phase{{Title: "Read", Detail: "two readers"}, {Title: "Write"}, {Title: "Check", Detail: "one‑checker"}}
	if r.ID != "wf_1" || r.Name != "demo" || r.Description != `Read, then "write", then check` || !reflect.DeepEqual(r.Phases, wantPhases) {
		t.Fatalf("run: %q %q %q %+v", r.ID, r.Name, r.Description, r.Phases)
	}
	if r.Status != workflow.Running || r.StartedMs != ms || r.EndedMs != 0 || r.Phase != "Write" {
		t.Fatalf("run state: %s started %d ended %d phase %q", r.Status, r.StartedMs, r.EndedMs, r.Phase)
	}
	want := []workflow.Agent{
		{ID: "a1", Label: "read:x", Phase: "Read", Status: workflow.Done, Model: "claude-opus-5-5", Tokens: 1503, ToolUses: 1,
			Activity: "# Map of x", StartedMs: ms, UpdatedMs: ms},
		{ID: "a2", Label: "read:y", Phase: "Read", Status: workflow.Running, Model: "claude-opus-5-5", Tokens: 1032, ToolUses: 1,
			Tool: "Read", Activity: "/w/y.go", StartedMs: ms, UpdatedMs: ms},
		{ID: "a3", Label: "write", Phase: "Write", Status: workflow.Running},
	}
	if !reflect.DeepEqual(r.Agents, want) {
		t.Fatalf("agents:\n%+v\nwant\n%+v", r.Agents, want)
	}
	if r.Counts != (workflow.Counts{Running: 2, Done: 1}) || r.Tokens != 2535 || r.ToolUses != 2 {
		t.Fatalf("totals: %+v %d %d", r.Counts, r.Tokens, r.ToolUses)
	}

	// A line still being written waits; once complete, it counts.
	a2 := filepath.Join(dir, "agent-a2.jsonl")
	partial := sideAssistant("m2", `[{"type":"tool_use","id":"toolu_c","name":"Grep","input":{"pattern":"TODO"}}]`, 2, 30)
	f, _ := os.OpenFile(a2, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(partial[:40])
	f.Close()
	if a := c.Workflows(s.transcript)[0].Agents[1]; a.ToolUses != 1 || a.Tool != "Read" {
		t.Fatalf("a partial line was read: %+v", a)
	}
	f, _ = os.OpenFile(a2, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(partial[40:] + "\n")
	f.Close()
	if a := c.Workflows(s.transcript)[0].Agents[1]; a.ToolUses != 2 || a.Tool != "Grep" || a.Activity != "TODO" {
		t.Fatalf("after the line was completed: %+v", a)
	}

	// A notification quoted by the agent is not how the run ended.
	quote, _ := json.Marshal(notification("wt1", "failed", "no", "", ""))
	s.append(s.transcript,
		`{"type":"assistant",`+stamp+`,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":`+string(quote)+`}}]}}`,
		`{"type":"user",`+stamp+`,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":`+string(quote)+`}]}}`)
	if r := c.Workflows(s.transcript)[0]; r.Status != workflow.Running {
		t.Fatalf("a quoted notification ended the run: %s", r.Status)
	}

	// The run ends: the notification carries a result cut short, the
	// output file all of it and the log.
	s.append(filepath.Join(dir, "journal.jsonl"), `{"type":"result","key":"v2:2","agentId":"a2","result":"y"}`)
	s.write(s.outputFile("wt1"), `{"summary":"Demo run","logs":["read done: 2/2"],"result":{"spec":"# Spec\nall of it"}}`)
	s.append(s.transcript, queued(notification("wt1", "completed", "Dynamic workflow &quot;Demo run&quot; completed",
		`{"spec":"# Spec &lt;cut&gt;`, s.outputFile("wt1"))))
	r = c.Workflows(s.transcript)[0]
	if r.Status != workflow.Completed || r.EndedMs != ms || r.Result != `{"spec":"# Spec\nall of it"}` || !reflect.DeepEqual(r.Logs, []string{"read done: 2/2"}) {
		t.Fatalf("ended run: %s ended %d result %q logs %q", r.Status, r.EndedMs, r.Result, r.Logs)
	}
	if r.Summary != `Dynamic workflow "Demo run" completed` {
		t.Fatalf("summary %q", r.Summary)
	}
	// An agent that never reported is stopped with its run.
	if r.Agents[2].Status != workflow.Stopped || r.Counts != (workflow.Counts{Done: 2, Stopped: 1}) {
		t.Fatalf("agents of the ended run: %+v %+v", r.Agents, r.Counts)
	}

	// Without an output file where Claude Code keeps it, the notification's
	// result stands.
	os.Remove(s.outputFile("wt1"))
	c2 := &claude{}
	if r := c2.Workflows(s.transcript)[0]; r.Result != `{"spec":"# Spec <cut>` || r.Logs != nil {
		t.Fatalf("result without the output file: %q %q", r.Result, r.Logs)
	}

	// A second run of a saved script failed at once: named by its launch.
	s.append(s.transcript, launchLine("wf_2", "wt2", "demo", "Demo run", filepath.Join(filepath.Dir(s.transcript), "..", "..", "x.js")))
	os.MkdirAll(s.runDir("wf_2"), 0o700)
	s.append(s.transcript, queued(notification("wt2", "failed", "Dynamic workflow &quot;Demo run&quot; failed: TypeError: x &lt; y", "", "")))
	byID := runsByID(c.Workflows(s.transcript))
	if r := byID["wf_2"]; r.Status != workflow.Failed || r.Name != "demo" || r.Description != "Demo run" ||
		r.Summary != `Dynamic workflow "Demo run" failed: TypeError: x < y` || len(r.Phases) != 0 || len(r.Agents) != 0 {
		t.Fatalf("failed run: %+v", r)
	}
}

func TestWorkflowsReplacedFile(t *testing.T) {
	s := newWFSession(t)
	c := &claude{}
	dir := s.runDir("wf_1")
	journal := filepath.Join(dir, "journal.jsonl")
	s.append(journal, `{"type":"started","agentId":"a1","label":"one"}`, `{"type":"started","agentId":"a2","label":"two"}`)
	if n := len(c.Workflows(s.transcript)[0].Agents); n != 2 {
		t.Fatalf("%d agents", n)
	}
	// Shorter than what was read: read again from the start.
	s.write(journal, `{"type":"started","agentId":"a3"}`+"\n")
	if a := c.Workflows(s.transcript)[0].Agents; len(a) != 1 || a[0].ID != "a3" {
		t.Fatalf("after the journal was replaced: %+v", a)
	}
	// Without a script or a launch, the run goes by its id.
	if r := c.Workflows(s.transcript)[0]; r.Name != "wf_1" || r.Status != workflow.Running {
		t.Fatalf("run: %+v", r)
	}
}

func TestWorkflowsNoSymlinks(t *testing.T) {
	s := newWFSession(t)
	c := &claude{}
	outside := t.TempDir()
	s.append(filepath.Join(outside, "journal.jsonl"), `{"type":"started","agentId":"a1"}`)
	s.write(filepath.Join(outside, "agent-a1.jsonl"), "secret\n")
	os.MkdirAll(filepath.Join(s.session, "subagents", "workflows"), 0o700)
	if err := os.Symlink(outside, s.runDir("wf_link")); err != nil {
		t.Fatal(err)
	}
	if runs := c.Workflows(s.transcript); len(runs) != 0 {
		t.Fatalf("a symlinked run dir was read: %+v", runs)
	}
	if _, _, ok := c.WorkflowTranscript(s.transcript, "wf_link", "a1"); ok {
		t.Fatal("transcript in a symlinked run dir")
	}

	dir := s.runDir("wf_1")
	s.append(filepath.Join(dir, "journal.jsonl"), `{"type":"started","agentId":"a1"}`)
	os.Symlink(filepath.Join(outside, "agent-a1.jsonl"), filepath.Join(dir, "agent-a1.jsonl"))
	if a := c.Workflows(s.transcript)[0].Agents[0]; a.StartedMs != 0 || a.Activity != "" {
		t.Fatalf("a symlinked agent transcript was read: %+v", a)
	}
	if _, _, ok := c.WorkflowTranscript(s.transcript, "wf_1", "a1"); ok {
		t.Fatal("symlinked agent transcript")
	}

	s.write(filepath.Join(dir, "agent-a2.jsonl"), "")
	p, parse, ok := c.WorkflowTranscript(s.transcript, "wf_1", "a2")
	if !ok || p != filepath.Join(dir, "agent-a2.jsonl") || parse == nil {
		t.Fatalf("WorkflowTranscript: %q %v", p, ok)
	}
	for _, bad := range [][2]string{{"..", "a2"}, {"wf_1", "../agent-a2"}, {"wf_1", "a2.jsonl"}, {"wf_1", ""}, {"", "a2"}, {"wf_1", "nope"}} {
		if _, _, ok := c.WorkflowTranscript(s.transcript, bad[0], bad[1]); ok {
			t.Errorf("WorkflowTranscript(%q, %q) ok", bad[0], bad[1])
		}
	}
}

func TestTaskOutputFile(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	uid := strconv.Itoa(os.Getuid())
	good := "/tmp/claude-" + uid + "/-home-flo-x/s1/tasks/wt1.output"
	if !taskOutputFile(good, "s1", "wt1") {
		t.Fatalf("%s rejected", good)
	}
	for _, bad := range []string{
		"/tmp/claude-" + uid + "/-home-flo-x/s2/tasks/wt1.output",
		"/tmp/claude-" + uid + "/-home-flo-x/s1/tasks/wt2.output",
		"/tmp/claude-" + uid + "/-home-flo-x/s1/other/wt1.output",
		"/tmp/claude-" + uid + "/../../home/flo/s1/tasks/wt1.output",
		"/home/flo/.claude/s1/tasks/wt1.output",
		"tmp/claude-" + uid + "/p/s1/tasks/wt1.output",
		"",
	} {
		if taskOutputFile(bad, "s1", "wt1") {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestScriptMeta(t *testing.T) {
	for _, tc := range []struct {
		src, name, desc string
		phases          []string
	}{
		{demoScript, "demo", `Read, then "write", then check`, []string{"Read", "Write", "Check"}},
		{"export const meta = {name:'a',phases:[]}", "a", "", nil},
		{"export  const meta={ \"name\": \"q\", description: 'it\\'s \\u00e9', }", "q", "it's é", nil},
		{"// x\nexport const meta = {\n name: `multi\nline`,\n}", "multi\nline", "", nil},
		{"export const meta = { name: `a ${b}` }", "", "", nil},
		{"export const meta = { name: 'open", "", "", nil},
		{"export const meta = { name: 'a' description: 'b' }", "", "", nil},
		{"export const meta = { name: 'a', phases: [{ detail: 'no title' }, { title: 'T', n: 3, ok: true, x: null }] }", "a", "", []string{"T"}},
		{"const meta = { name: 'not exported' }", "", "", nil},
		{"export const meta = " + strings.Repeat("[", 100), "", "", nil},
	} {
		name, desc, phases := scriptMeta([]byte(tc.src))
		var titles []string
		for _, p := range phases {
			titles = append(titles, p.Title)
		}
		if name != tc.name || desc != tc.desc || !reflect.DeepEqual(titles, tc.phases) || phases == nil {
			t.Errorf("scriptMeta(%q) = %q %q %v", tc.src, name, desc, phases)
		}
	}
}

func TestParseSidechain(t *testing.T) {
	task := sideLine("user", `{"role":"user","content":"[Workflow harness — computed task] Header. The computed task text follows:\n  Map x.\n    indented\n  done"}`)
	if got := parseLine([]byte(task)); got != nil {
		t.Fatalf("parseLine shows a sidechain: %+v", got)
	}
	want := Es{{Kind: transcript.User, Text: "Map x.\n  indented\ndone", TimeMs: ms}}
	if got := parseSidechain([]byte(task)); !reflect.DeepEqual(got, want) {
		t.Fatalf("task: %+v", got)
	}
	call := sideAssistant("m1", `[{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"ls"}}]`, 1, 1)
	if got := parseSidechain([]byte(call)); len(got) != 1 || got[0].Kind != transcript.Tool || got[0].Text != "ls" {
		t.Fatalf("tool call: %+v", got)
	}
}

func TestWorkflowToolSummary(t *testing.T) {
	script, _ := json.Marshal(demoScript)
	for _, tc := range []struct{ input, want string }{
		{`{"script":` + string(script) + `}`, `demo: Read, then "write", then check`},
		{`{"name":"review"}`, "review"},
		{`{"scriptPath":"/p/s1/workflows/scripts/demo-wf_1.js","resumeFromRunId":"wf_1"}`, "demo-wf_1.js"},
	} {
		if e := toolUse("t", "Workflow", json.RawMessage(tc.input)); e.Text != tc.want {
			t.Errorf("Workflow %s: %q, want %q", tc.input, e.Text, tc.want)
		}
	}
	if e := toolUse("t", "Workflow", json.RawMessage(`{"script":`+string(script)+`}`)); e.Detail != demoScript {
		t.Errorf("Workflow detail %q", e.Detail)
	}
}

func TestStructuredSummary(t *testing.T) {
	for in, want := range map[string]string{
		`{"summary":"Built it.\nDetails","files":["a"]}`: "Built it.",
		`{"verdict":"","b":"second","a":"first"}`:        "first",
		`{"n":3}`: `{"n":3}`,
	} {
		if got := structuredSummary(json.RawMessage(in)); got != want {
			t.Errorf("structuredSummary(%s) = %q, want %q", in, got, want)
		}
	}
}

// TestRealWorkflows reads every workflow run this machine's Claude Code
// recorded, to check the reader against real data after a Claude Code
// update. It prints nothing of their content.
func TestRealWorkflows(t *testing.T) {
	if os.Getenv("FLEET_REAL_TRANSCRIPTS") == "" {
		t.Skip("set FLEET_REAL_TRANSCRIPTS=1 to read this machine's workflow runs")
	}
	a := New("", nil)
	w := a.(adapter.Workflower)
	dirs, _ := filepath.Glob(filepath.Join(a.(adapter.Transcripter).TranscriptDir(""), "*", "*", "subagents", "workflows"))
	statuses := map[string]int{}
	agents, unnamed := 0, 0
	for _, d := range dirs {
		transcriptPath := filepath.Dir(filepath.Dir(d)) + ".jsonl"
		for _, r := range w.Workflows(transcriptPath) {
			statuses[r.Status]++
			if r.Name == r.ID {
				unnamed++
			}
			n := r.Counts.Running + r.Counts.Done + r.Counts.Failed + r.Counts.Stopped
			if n != len(r.Agents) {
				t.Errorf("%s: counts %+v for %d agents", r.ID, r.Counts, len(r.Agents))
			}
			for _, ag := range r.Agents {
				agents++
				p, parse, ok := w.WorkflowTranscript(transcriptPath, r.ID, ag.ID)
				if !ok {
					continue // not written (yet)
				}
				pg, err := transcript.Forward(p, 0, parse)
				if err != nil {
					t.Fatalf("%s: %v", p, err)
				}
				for _, e := range pg.Entries {
					checkEntry(t, e)
				}
			}
		}
	}
	t.Logf("%d sessions, runs %v (%d without a name), %d agents", len(dirs), statuses, unnamed, agents)
}
