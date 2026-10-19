package web

import (
	"context"
	"path/filepath"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func (s *fakeSource) Interrupt(_ context.Context, agent string) (*fleetv1.Agent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || agent != s.agent.Id {
		return nil, false, &Error{Status: 404, Msg: "no agent " + agent}
	}
	if s.agent.State != fleetv1.AgentState_AGENT_STATE_WORKING {
		return nil, false, &Error{Status: 400, Msg: "agent " + agent + " is not working"}
	}
	s.agent.State = fleetv1.AgentState_AGENT_STATE_IDLE
	return s.agent, true, nil
}

func TestInterruptRoute(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "claude-api-1", State: fleetv1.AgentState_AGENT_STATE_WORKING}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/interrupt", "wrong", "", &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	var r struct {
		Agent       Agent
		Interrupted bool
	}
	if code := api(t, ts, "POST", "/api/agents/a1/interrupt", tok, "", &r); code != 200 || !r.Interrupted || r.Agent.State != "idle" {
		t.Fatalf("interrupt: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/interrupt", tok, "", &e); code != 400 {
		t.Fatalf("interrupt an idle agent: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/agents/nope/interrupt", tok, "", &e); code != 404 {
		t.Fatalf("unknown agent: %d %+v", code, e)
	}
	if !forwardable("POST", "agents/a1/interrupt") {
		t.Fatal("an interrupt of another fleet's agent is not forwarded")
	}
}
