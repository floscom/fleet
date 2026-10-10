package web

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
)

// Git records the request and answers with the agent's git status.
func (s *fakeSource) Git(_ context.Context, req *fleetv1.GitRequest) (*fleetv1.GitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || req.Agent != s.agent.Id {
		return nil, &Error{Status: 404, Msg: "no agent " + req.Agent}
	}
	s.gits = append(s.gits, proto.Clone(req).(*fleetv1.GitRequest))
	return &fleetv1.GitResponse{Status: s.agent.Git, Output: "done"}, nil
}

func TestGitRoute(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Git: &fleetv1.GitStatus{Branch: "fleet/x", Upstream: "origin/fleet/x", Ahead: 2}}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/git", "wrong", `{"action":"push"}`, &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	var r struct {
		Git    *GitStatus
		Output string
	}
	if code := api(t, ts, "POST", "/api/agents/a1/git", tok, `{"action":"pull","mode":"rebase","fromBase":true}`, &r); code != 200 ||
		r.Git == nil || r.Git.Branch != "fleet/x" || r.Git.Ahead != 2 || r.Output != "done" {
		t.Fatalf("pull: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/git", tok, `{"action":"push","force":true}`, &r); code != 200 {
		t.Fatalf("push: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/git", tok, `{}`, &r); code != 200 {
		t.Fatalf("status: %d %+v", code, r)
	}
	want := []*fleetv1.GitRequest{
		{Agent: "a1", Action: fleetv1.GitAction_GIT_ACTION_PULL, PullMode: fleetv1.PullMode_PULL_MODE_REBASE, FromBase: true},
		{Agent: "a1", Action: fleetv1.GitAction_GIT_ACTION_PUSH, Force: true},
		{Agent: "a1"},
	}
	if len(src.gits) != len(want) {
		t.Fatalf("requests %v, want %v", src.gits, want)
	}
	for i := range want {
		if !proto.Equal(src.gits[i], want[i]) {
			t.Fatalf("request %d: %v, want %v", i, src.gits[i], want[i])
		}
	}
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/api/agents/zz/git", `{"action":"fetch"}`, 404},
		{"/api/agents/a1/git", `{"action":"commit"}`, 400},
		{"/api/agents/a1/git", `{"action":"unspecified"}`, 400},
		{"/api/agents/a1/git", `{"action":"pull","mode":"octopus"}`, 400},
		{"/api/agents/a1/git", `not json`, 400},
	} {
		if code := api(t, ts, "POST", c.path, tok, c.body, &e); code != c.code || e.Error == "" {
			t.Fatalf("%s %s: %d %+v, want %d", c.path, c.body, code, e, c.code)
		}
	}
	if !forwardable("POST", "agents/a1/git") || forwardTimeout("POST", "agents/a1/git") != gitTimeout {
		t.Fatal("git route is not forwarded to peers with its timeout")
	}
}
