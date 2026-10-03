package claude

// Subagents a session starts with the Agent tool, outside of any workflow.
// Claude Code 2.1.280 records each next to the session's transcript
// (<session> is its path without ".jsonl"):
//
//	<session>/subagents/agent-<agent id>.jsonl
//	    its transcript: the session transcript's format, every line a
//	    sidechain
//	<session>/subagents/agent-<agent id>.meta.json
//	    {"agentType", "description", "name", "toolUseId", "requestShape":
//	    "background"|"foreground", "taskKind", ...}; a teammate
//	    ("taskKind":"in_process_teammate") has no toolUseId
//
// Nothing records that one finished: it did once its latest message ended
// the turn. A teammate then idles until it is sent a message, and starts
// over. A background agent's task (its id the agent's) notifies the session
// each time it stops, also when it failed or was stopped.
//
// The dashboard shows them as workflow runs (see agentRuns): agents that
// worked at the same time make one run.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fleet/internal/workflow"
)

// agentRunPrefix starts the ids of the runs agentRuns makes up; workflow
// run ids start with "wf_".
const agentRunPrefix = "agents-"

// agentFiles lists the ids of the session's subagents, by their
// transcripts.
func agentFiles(session string) []string {
	var ids []string
	for _, e := range readDir(filepath.Join(session, "subagents")) {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl"); validID(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// agentRuns reads the session's subagents and groups them into runs: an
// agent that started while another of a run still worked joins that run.
func (c *claude) agentRuns(session string, ids []string, ends sessionScan) []workflow.Run {
	dir := filepath.Join(session, "subagents")
	var agents []workflow.Agent
	for _, id := range ids {
		a := workflow.Agent{ID: id, Status: workflow.Running}
		ended := false
		ok := c.wf.follow(filepath.Join(dir, "agent-"+id+".jsonl"), func() lineReader { return &agentScan{} }, func(r lineReader) {
			s := r.(*agentScan)
			s.fill(&a)
			ended = s.ended
		})
		if !ok {
			continue
		}
		a.Label, a.Call = c.wf.subagentMeta(filepath.Join(dir, "agent-"+id+".meta.json"))
		if ended {
			a.Status = workflow.Done
		}
		// The task's latest notification, unless the agent went on after.
		if e, ok := ends.agentEnds[id]; ok && e.ms >= a.UpdatedMs {
			switch e.status {
			case workflow.Completed:
				a.Status = workflow.Done
			default:
				a.Status = e.status
			}
		}
		agents = append(agents, a)
	}
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].StartedMs < agents[j].StartedMs })

	var runs []workflow.Run
	var until int64 // when the agents of the latest run stopped working; -1 while one works
	for _, a := range agents {
		if len(runs) == 0 || until >= 0 && a.StartedMs > until {
			runs = append(runs, workflow.Run{ID: agentRunPrefix + a.ID, Phases: []workflow.Phase{}, StartedMs: a.StartedMs})
			until = 0
		}
		r := &runs[len(runs)-1]
		r.Agents = append(r.Agents, a)
		if a.Status == workflow.Running {
			until = -1
		} else if until >= 0 {
			until = max(until, a.UpdatedMs)
		}
	}
	for i := range runs {
		r := &runs[i]
		r.Status = workflow.Completed
		for _, a := range r.Agents {
			if a.Status == workflow.Running {
				r.Status = workflow.Running
			}
		}
		r.Tally()
		if r.Status != workflow.Running {
			r.EndedMs = r.UpdatedMs
		}
		if len(r.Agents) == 1 {
			r.Name = r.Agents[0].Label
		} else {
			labels := make([]string, len(r.Agents))
			for j, a := range r.Agents {
				labels[j] = a.Label
				if labels[j] == "" {
					labels[j] = a.ID
				}
			}
			r.Name = fmt.Sprintf("%d agents", len(r.Agents))
			r.Description = strings.Join(labels, " · ")
		}
		if r.Name == "" {
			r.Name = "agent"
		}
	}
	return runs
}

// agentTranscript returns the path of subagent agent's transcript.
func agentTranscript(session, agent string) (string, bool) {
	dir := filepath.Join(session, "subagents")
	p := filepath.Join(dir, "agent-"+agent+".jsonl")
	return p, noLinks(dir, p)
}

// subagentMeta reads what a subagent is for, and the Agent call that
// started it, from its meta file.
func (c *workflowCache) subagentMeta(path string) (label, call string) {
	if !noLinks(path) {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 {
		return "", ""
	}
	var m struct {
		Description string `json:"description"`
		Name        string `json:"name"`
		AgentType   string `json:"agentType"`
		ToolUseID   string `json:"toolUseId"`
	}
	_ = json.Unmarshal(data, &m)
	for _, s := range []string{m.Description, m.Name, m.AgentType} {
		if s = strings.TrimSpace(s); s != "" {
			return s, m.ToolUseID
		}
	}
	return "", m.ToolUseID
}
