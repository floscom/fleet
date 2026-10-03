package claude

import (
	"path/filepath"
	"strings"
	"testing"

	"fleet/internal/workflow"
)

// subLine is a line of a subagent's transcript, written at hh:mm.
func subLine(at, typ, msg string) string {
	return `{"isSidechain":true,"timestamp":"2026-09-27T` + at + `:00.000Z","type":"` + typ + `","message":` + msg + `}`
}

func subAssistant(at, stop, content string) string {
	return subLine(at, "assistant", `{"model":"claude-opus-5-5","role":"assistant","stop_reason":`+stop+`,"content":`+content+`}`)
}

func TestAgentRuns(t *testing.T) {
	s := newWFSession(t)
	c := &claude{}
	dir := filepath.Join(s.session, "subagents")
	task := subLine("10:00", "user", `{"role":"user","content":"Do it"}`)
	bash := `[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{"command":"make"}}]`
	done := `[{"type":"text","text":"All fixed."}]`

	// Two background agents at work together, a teammate that finished.
	s.write(filepath.Join(dir, "agent-a1.meta.json"), `{"agentType":"general-purpose","description":"Fix the API","toolUseId":"toolu_1","requestShape":"background"}`)
	s.append(filepath.Join(dir, "agent-a1.jsonl"), task, subAssistant("10:01", `"end_turn"`, done))
	s.write(filepath.Join(dir, "agent-a2.meta.json"), `{"agentType":"general-purpose","description":"Fix the UI","toolUseId":"toolu_2"}`)
	s.append(filepath.Join(dir, "agent-a2.jsonl"), subLine("10:00", "user", `{"role":"user","content":"Go"}`),
		subAssistant("10:05", `"tool_use"`, bash))
	// Not subagents.
	s.write(filepath.Join(dir, "agent-a3.txt"), "")
	s.write(filepath.Join(dir, "agent-..jsonl"), "")

	runs := c.Workflows(s.transcript)
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	r := runs[0]
	if r.ID != "agents-a1" || r.Name != "2 agents" || r.Description != "Fix the API · Fix the UI" || r.Status != workflow.Running || r.EndedMs != 0 {
		t.Fatalf("first run: %+v", r)
	}
	if a := r.Agents[0]; a.Status != workflow.Done || a.Call != "toolu_1" || a.Label != "Fix the API" {
		t.Fatalf("a1: %+v", a)
	}
	if a := r.Agents[1]; a.Status != workflow.Running || a.Tool != "Bash" {
		t.Fatalf("a2: %+v", a)
	}
	if p, _, ok := c.WorkflowTranscript(s.transcript, "agents-a1", "a2"); !ok || !strings.HasSuffix(p, "/subagents/agent-a2.jsonl") {
		t.Fatalf("transcript of a2: %q %v", p, ok)
	}

	// a2 is stopped: its task's notification is the latest of it.
	s.append(s.transcript, strings.Replace(queued(notification("a2", "killed", `Agent "Fix the UI" was stopped`, "", "")), "10:00:00.250", "10:06:00.000", 1))
	r = c.Workflows(s.transcript)[0]
	if r.Agents[1].Status != workflow.Stopped || r.Status != workflow.Completed {
		t.Fatalf("after a2 was stopped: %+v", r)
	}

	// Later, alone: a teammate (no call id), idle after its turn.
	s.write(filepath.Join(dir, "agent-aqa-1.meta.json"), `{"agentType":"qa","name":"qa","description":"QA pass","taskKind":"in_process_teammate"}`)
	s.append(filepath.Join(dir, "agent-aqa-1.jsonl"), subLine("11:00", "user", `{"role":"user","content":"Check"}`),
		subAssistant("11:02", "null", `[{"type":"thinking","thinking":""}]`),
		subAssistant("11:02", `"end_turn"`, done))
	runs = c.Workflows(s.transcript)
	if len(runs) != 2 {
		t.Fatalf("runs with the teammate: %+v", runs)
	}
	if q := runs[1]; q.ID != "agents-aqa-1" || q.Name != "QA pass" || q.Status != workflow.Completed || q.EndedMs == 0 || q.Agents[0].Call != "" {
		t.Fatalf("teammate run: %+v", q)
	}
	// A teammate sent a message works again: its run with it.
	s.append(filepath.Join(dir, "agent-aqa-1.jsonl"), subLine("11:30", "user", `{"role":"user","content":"Again"}`),
		subAssistant("11:31", `"tool_use"`, bash))
	if q := c.Workflows(s.transcript)[1]; q.Status != workflow.Running || q.Agents[0].Status != workflow.Running {
		t.Fatalf("woken teammate: %+v", q)
	}
}
