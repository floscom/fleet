package web

import (
	"path/filepath"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func (s *fakeSource) EditQueue(req *fleetv1.EditQueueRequest) (*fleetv1.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || req.Agent != s.agent.Id {
		return nil, &Error{Status: 404, Msg: "no agent " + req.Agent}
	}
	switch op := req.Op.(type) {
	case *fleetv1.EditQueueRequest_Add:
		s.agent.Queue = append(s.agent.Queue, &fleetv1.QueuedMessage{Id: op.Add, Text: op.Add})
	case *fleetv1.EditQueueRequest_Remove:
		s.agent.Queue = nil
	case *fleetv1.EditQueueRequest_Paused:
		s.agent.QueuePaused = op.Paused
	}
	return s.agent, nil
}

func TestQueueRoute(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "claude-api-1", State: fleetv1.AgentState_AGENT_STATE_WORKING}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/queue", "wrong", `{"add":"x"}`, &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	for _, body := range []string{`{}`, `{"add":"x","paused":true}`} {
		if code := api(t, ts, "POST", "/api/agents/a1/queue", tok, body, &e); code != 400 {
			t.Fatalf("%s: %d %+v", body, code, e)
		}
	}
	var r struct{ Agent Agent }
	if code := api(t, ts, "POST", "/api/agents/a1/queue", tok, `{"add":"write the tests"}`, &r); code != 200 ||
		len(r.Agent.Queue) != 1 || r.Agent.Queue[0].Text != "write the tests" {
		t.Fatalf("add: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/queue", tok, `{"paused":true}`, &r); code != 200 || !r.Agent.QueuePaused {
		t.Fatalf("pause: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/queue", tok, `{"remove":"write the tests"}`, &r); code != 200 || len(r.Agent.Queue) != 0 {
		t.Fatalf("remove: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/nope/queue", tok, `{"add":"x"}`, &e); code != 404 {
		t.Fatalf("unknown agent: %d %+v", code, e)
	}
}
