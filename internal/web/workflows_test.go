package web

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/workflow"
)

func TestWorkflowRoutes(t *testing.T) {
	now := time.Now().UnixMilli()
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	src.wfRuns = []workflow.Run{
		{ID: "wf_old", Status: workflow.Completed, EndedMs: now - (recentRun + time.Minute).Milliseconds(), Result: "r"},
		{ID: "wf_recent", Status: workflow.Failed, EndedMs: now - time.Minute.Milliseconds(), Summary: "boom", Result: "r", Logs: []string{"l"}},
		{ID: "wf_1", Status: workflow.Running, Agents: []workflow.Agent{{ID: "s1", Status: workflow.Running}}},
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "s1.jsonl")
	os.WriteFile(file, []byte("task\nworking\n"), 0o600)
	src.wfChats = map[string]Chat{"wf_1/s1": {Path: file, Parse: lineParser}}
	pathTok := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(pathTok)
	ts := startServerToken(t, src, pathTok)

	for _, p := range []string{"/api/workflows", "/api/agents/a1/workflows", "/api/agents/a1/workflows/wf_1/agents/s1/chat"} {
		if code := api(t, ts, "GET", p, "wrong", "", nil); code != 401 {
			t.Fatalf("%s without the token: %d", p, code)
		}
	}

	// Going on now: the running run and the one that ended lately, without
	// their results.
	var list struct {
		Workflows []struct {
			Agent string
			Run   workflow.Run
		}
	}
	if code := api(t, ts, "GET", "/api/workflows", tok, "", &list); code != 200 || len(list.Workflows) != 2 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if w := list.Workflows[0]; w.Agent != "a1" || w.Run.ID != "wf_recent" || w.Run.Summary != "boom" || w.Run.Result != "" || w.Run.Logs != nil {
		t.Fatalf("recent run: %+v", w)
	}
	if w := list.Workflows[1]; w.Run.ID != "wf_1" || len(w.Run.Agents) != 1 {
		t.Fatalf("running run: %+v", w)
	}

	// An agent's runs, whole; held with wait=1 until they change.
	var r workflowsReply
	if code := api(t, ts, "GET", "/api/agents/a1/workflows", tok, "", &r); code != 200 || len(r.Runs) != 3 || r.Runs[0].Result != "r" || r.V == "" || r.Agent.ID != "a1" {
		t.Fatalf("runs: %d %+v", code, r)
	}
	if code := api(t, ts, "GET", "/api/agents/nope/workflows", tok, "", nil); code != 404 {
		t.Fatalf("runs of an unknown agent: %d", code)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		src.mu.Lock()
		src.wfRuns[2].Agents[0].Status = workflow.Done
		src.mu.Unlock()
	}()
	start := time.Now()
	var next workflowsReply
	api(t, ts, "GET", "/api/agents/a1/workflows?wait=1&v="+r.V, tok, "", &next)
	if waited := time.Since(start); waited < time.Second || next.V == r.V || next.Runs[2].Agents[0].Status != workflow.Done {
		t.Fatalf("wait for a change: after %v: %+v", waited, next)
	}
	// A version the page does not have: answered at once.
	start = time.Now()
	api(t, ts, "GET", "/api/agents/a1/workflows?wait=1&v=old", tok, "", &next)
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("stale version waited %v", waited)
	}

	// A workflow agent's conversation, like an agent's.
	var ch chatResp
	if code := api(t, ts, "GET", "/api/agents/a1/workflows/wf_1/agents/s1/chat", tok, "", &ch); code != 200 || !ch.Reset || !reflect.DeepEqual(texts(ch.Entries), []string{"task", "working"}) {
		t.Fatalf("workflow agent chat: %d %+v", code, ch)
	}
	if code := api(t, ts, "GET", "/api/agents/a1/workflows/wf_1/agents/s2/chat", tok, "", nil); code != 404 {
		t.Fatalf("unknown workflow agent: %d", code)
	}

	// Finished agents have no runs going on.
	src.mu.Lock()
	src.agent.State = fleetv1.AgentState_AGENT_STATE_EXITED
	src.mu.Unlock()
	if code := api(t, ts, "GET", "/api/workflows", tok, "", &list); code != 200 || len(list.Workflows) != 0 {
		t.Fatalf("list of a finished agent: %d %+v", code, list)
	}
}

func TestForwardableWorkflowRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, rest string
		want         bool
	}{
		{"GET", "workflows", true},
		{"GET", "agents/a1/workflows", true},
		{"GET", "agents/a1/workflows/wf_1/agents/s1/chat", true},
		{"GET", "agents/a1/workflows/wf_1/agents/s1/screen", false},
		{"GET", "agents/a1/workflows/wf_1/agents//chat", false},
		{"GET", "agents/a1/workflows//agents/s1/chat", false},
		{"GET", "agents/a1/workflows/wf_1/agents/s1/chat/more", false},
		{"POST", "agents/a1/workflows", false},
		{"POST", "workflows", false},
	} {
		if got := forwardable(tc.method, tc.rest); got != tc.want {
			t.Errorf("forwardable(%s %s) = %v", tc.method, tc.rest, got)
		}
	}
}
