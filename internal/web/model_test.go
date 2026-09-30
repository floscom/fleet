package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

type fakeSwitch struct{ agent, model, effort string }

var testModel = Model{
	Model: "opus", Name: "claude-opus-5-5", Effort: "high", Switch: true,
	Models:  []ModelChoice{{ID: "opus", Label: "Opus", Efforts: []string{"low", "high"}}, {ID: "haiku", Label: "Haiku", Efforts: []string{}}},
	Efforts: []EffortChoice{{ID: "low", Label: "Low"}, {ID: "high", Label: "High"}},
}

func (s *fakeSource) SwitchModel(_ context.Context, agent, model, effort string) (*Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent != "a1" {
		return nil, &Error{Status: 404, Msg: "no agent " + agent}
	}
	if model == "gpt" {
		return nil, &Error{Status: 400, Msg: "Claude Code has no model \"gpt\""}
	}
	s.switches = append(s.switches, fakeSwitch{agent, model, effort})
	m := testModel
	if model != "" {
		m.Model, m.Name = model, ""
	}
	if effort != "" {
		m.Effort = effort
	}
	return &m, nil
}

func TestSwitchModel(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", Name: "claude-api-1", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/agents/a1/model", "wrong", `{"model":"haiku"}`, &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	var r struct{ Model Model }
	if code := api(t, ts, "POST", "/api/agents/a1/model", tok, `{"model":"haiku","effort":"low"}`, &r); code != 200 ||
		r.Model.Model != "haiku" || r.Model.Effort != "low" || !r.Model.Switch || len(r.Model.Models) != 2 {
		t.Fatalf("switch: %d %+v", code, r)
	}
	if code := api(t, ts, "POST", "/api/agents/a1/model", tok, `{"effort":"high"}`, &r); code != 200 || r.Model.Effort != "high" {
		t.Fatalf("switch effort: %d %+v", code, r)
	}
	if want := []fakeSwitch{{"a1", "haiku", "low"}, {"a1", "", "high"}}; !reflect.DeepEqual(src.switches, want) {
		t.Fatalf("switches %+v, want %+v", src.switches, want)
	}
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/api/agents/zz/model", `{"model":"haiku"}`, 404},
		{"/api/agents/a1/model", `{"model":"gpt"}`, 400},
		{"/api/agents/a1/model", `not json`, 400},
	} {
		if code := api(t, ts, "POST", c.path, tok, c.body, &e); code != c.code || e.Error == "" {
			t.Fatalf("%s %s: %d %+v, want %d", c.path, c.body, code, e, c.code)
		}
	}
}

func TestRunWithModel(t *testing.T) {
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1"}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)
	var run struct{ Agent Agent }
	if code := api(t, ts, "POST", "/api/agents", tok, `{"adapter":"claude","root":"code","model":" fable ","effort":"max"}`, &run); code != 201 {
		t.Fatalf("run: %d %+v", code, run)
	}
	if len(src.runs) != 1 || src.runs[0].Model != "fable" || src.runs[0].Effort != "max" {
		t.Fatalf("run request %v", src.runs)
	}
}

func TestChatModel(t *testing.T) {
	file := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(file, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := newFakeSource()
	src.agent = &fleetv1.Agent{Id: "a1", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	m := testModel
	src.chat = Chat{Path: file, Parse: lineParser, Model: &m}
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, src, path)
	var r struct{ Model *Model }
	if code := api(t, ts, "GET", "/api/agents/a1/chat", tok, "", &r); code != 200 || r.Model == nil || !reflect.DeepEqual(*r.Model, testModel) {
		t.Fatalf("chat: %d %+v", code, r.Model)
	}
	src.mu.Lock()
	src.chat.Model = nil
	src.mu.Unlock()
	var raw map[string]any
	if code := api(t, ts, "GET", "/api/agents/a1/chat", tok, "", &raw); code != 200 {
		t.Fatalf("chat: %d", code)
	}
	if _, ok := raw["model"]; ok {
		t.Fatalf("chat of an adapter without models has a model: %v", raw["model"])
	}
}

func TestForwardModel(t *testing.T) {
	if !forwardable(http.MethodPost, "agents/a1/model") || forwardable(http.MethodGet, "agents/a1/model") {
		t.Fatal("POST agents/<id>/model should be forwarded, GET not")
	}
	if forwardTimeout(http.MethodPost, "agents/a1/model") != modelTimeout {
		t.Fatal("a forwarded switch should get modelTimeout")
	}
}
