package web

import (
	"path/filepath"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func (s *fakeSource) SetNote(agent, note string) (*fleetv1.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || agent != s.agent.Id {
		return nil, &Error{Status: 404, Msg: "no agent " + agent}
	}
	s.agent.Note = note
	return s.agent, nil
}

func TestNoteRoute(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "claude-api-1", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/note", "wrong", `{"note":"x"}`, &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/note", tok, `{}`, &e); code != 400 {
		t.Fatalf("no note: %d %+v", code, e)
	}
	var r struct{ Agent Agent }
	if code := api(t, ts, "POST", "/api/agents/a1/note", tok, `{"note":"customer is testing, keep it open"}`, &r); code != 200 ||
		r.Agent.Note != "customer is testing, keep it open" {
		t.Fatalf("set: %d %+v", code, r.Agent)
	}
	var list struct{ Agents []Agent }
	if code := api(t, ts, "GET", "/api/agents", tok, "", &list); code != 200 || len(list.Agents) != 1 || list.Agents[0].Note == "" {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/note", tok, `{"note":""}`, &r); code != 200 || r.Agent.Note != "" {
		t.Fatalf("clear: %d %+v", code, r.Agent)
	}
	if code := api(t, ts, "POST", "/api/agents/nope/note", tok, `{"note":"x"}`, &e); code != 404 {
		t.Fatalf("unknown agent: %d %+v", code, e)
	}
	if !forwardable("POST", "agents/a1/note") {
		t.Fatal("a note on another fleet's agent is not forwarded")
	}
}
