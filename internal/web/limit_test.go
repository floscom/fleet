package web

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

func (s *fakeSource) SetAutoResume(_ context.Context, agent string, auto *bool, now bool) (*fleetv1.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || agent != s.agent.Id {
		return nil, &Error{Status: 404, Msg: "no agent " + agent}
	}
	a := s.agent
	if a.UsageLimit == nil {
		a.UsageLimit = &fleetv1.UsageLimit{}
	}
	if auto != nil {
		a.UsageLimit.AutoResume = *auto
	}
	if now {
		a.UsageLimit.Detail = "resumed"
	}
	return a, nil
}

func TestResumeRoute(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "codex-api-1", State: fleetv1.AgentState_AGENT_STATE_IDLE,
		UsageLimit: &fleetv1.UsageLimit{Window: "5h", ResetsAtMs: 1000, ResumeAtMs: 61000, AutoResume: true}}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/resume", "wrong", `{"auto":false}`, &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/resume", tok, `{}`, &e); code != 400 {
		t.Fatalf("empty: %d %+v", code, e)
	}
	var r struct{ Agent Agent }
	if code := api(t, ts, "POST", "/api/agents/a1/resume", tok, `{"auto":false}`, &r); code != 200 ||
		r.Agent.UsageLimit == nil || r.Agent.UsageLimit.AutoResume || r.Agent.UsageLimit.Window != "5h" {
		t.Fatalf("auto off: %d %+v", code, r.Agent.UsageLimit)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/resume", tok, `{"now":true}`, &r); code != 200 || r.Agent.UsageLimit.Detail != "resumed" {
		t.Fatalf("now: %d %+v", code, r.Agent.UsageLimit)
	}
	if code := api(t, ts, "POST", "/api/agents/nope/resume", tok, `{"now":true}`, &e); code != 404 {
		t.Fatalf("unknown agent: %d %+v", code, e)
	}
}

func TestLimitNotice(t *testing.T) {
	resume := time.Now().Add(time.Hour).Truncate(time.Minute)
	a := Agent{ID: "a1", Name: "codex-api-1", Cwd: "/src/api", State: "idle",
		UsageLimit: &AgentLimit{Window: "5h", ResumeAtMs: resume.UnixMilli(), AutoResume: true}}
	n, ok := limitNotice(nil, a)
	if !ok || n.Title != "codex-api-1 hit a usage limit" || n.Body != "api · 5h limit · resumes "+resume.Format("15:04") {
		t.Fatalf("stopped: %v %+v", ok, n)
	}
	if _, ok := limitNotice(a.UsageLimit, a); ok {
		t.Fatal("told twice")
	}
	gaveUp := a
	gaveUp.UsageLimit = &AgentLimit{Window: "5h", Detail: "auto-resume gave up: no reaction to 3 tries"}
	if n, ok := limitNotice(a.UsageLimit, gaveUp); !ok || !strings.HasSuffix(n.Body, "no reaction to 3 tries") {
		t.Fatalf("gave up: %v %+v", ok, n)
	}
	off := a
	off.UsageLimit = &AgentLimit{}
	if n, ok := limitNotice(nil, off); !ok || n.Body != "api · auto-resume off" {
		t.Fatalf("off: %v %+v", ok, n)
	}
	if _, ok := limitNotice(a.UsageLimit, Agent{ID: "a1", State: "working"}); ok {
		t.Fatal("told about a resumed agent")
	}
}

// An agent stopped by a limit gets the limit notice, never "is done".
func TestLimitedAgentIsNotDone(t *testing.T) {
	var got []Notice
	h := &hub{done: map[string]*time.Timer{}, doneWait: 10 * time.Millisecond}
	h.notify = func(n Notice) { got = append(got, n) }
	h.mu.Lock()
	h.noticeLocked(agentEntry{state: "working"}, Agent{ID: "a1", Name: "x", State: "idle"})
	h.noticeLocked(agentEntry{state: "idle"}, Agent{ID: "a1", Name: "x", State: "idle", UsageLimit: &AgentLimit{AutoResume: true, ResumeAtMs: 1}})
	h.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(got) != 1 || got[0].Title != "x hit a usage limit" {
		t.Fatalf("notices: %+v", got)
	}
}
