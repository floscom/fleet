package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/web"
)

// modelAdapter is testAdapter with a choice of models: "big" runs at "lo"
// or "hi" effort, "tiny" at none. It passes them to the script as
// $TEST_MODEL and $TEST_EFFORT; transcript lines "model=<m> effort=<e>"
// name them; SwitchModel types "switched <m> <e>".
type modelAdapter struct{ testAdapter }

func (modelAdapter) ID() string { return "modeled" }

func (modelAdapter) Models() adapter.Models {
	return adapter.Models{
		Models:  []adapter.Model{{ID: "big", Label: "Big", Efforts: []string{"lo", "hi"}}, {ID: "tiny", Label: "Tiny"}},
		Efforts: []adapter.Choice{{ID: "lo", Label: "Low"}, {ID: "hi", Label: "High"}},
	}
}

func (modelAdapter) ModelLine(line []byte, m *adapter.SessionModel) {
	s, ok := strings.CutPrefix(string(line), "model=")
	if !ok {
		return
	}
	name, effort, _ := strings.Cut(s, " effort=")
	m.Model, m.Name, m.Effort = "", name, effort
	if name == "big" || name == "tiny" {
		m.Model = name
	}
}

func (a modelAdapter) Launch(ctx context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	spec, err := a.testAdapter.Launch(ctx, req)
	if err == nil {
		spec.Env["TEST_MODEL"], spec.Env["TEST_EFFORT"] = req.Model, req.Effort
	}
	return spec, err
}

func (modelAdapter) SwitchModel(ctx context.Context, term adapter.Terminal, model, effort string) error {
	return term.Type(ctx, "switched "+model+" "+effort, true)
}

func TestCheckModel(t *testing.T) {
	for _, c := range []struct {
		ad            adapter.Adapter
		model, effort string
		ok            bool
	}{
		{modelAdapter{}, "", "", true},
		{testAdapter{}, "", "", true},
		{testAdapter{}, "big", "", false},
		{modelAdapter{}, "big", "hi", true},
		{modelAdapter{}, "claude-opus-5-5[1m]", "lo", true}, // any name the CLI may take
		{modelAdapter{}, "--dangerous", "", false},
		{modelAdapter{}, "big", "max", false},
		{modelAdapter{}, "tiny", "lo", false},
	} {
		if err := checkModel(c.ad, c.model, c.effort); (err == nil) != c.ok {
			t.Errorf("%s %q %q: %v", c.ad.ID(), c.model, c.effort, err)
		}
	}
}

func TestWebModel(t *testing.T) {
	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Adapter: "modeled", Root: "code", Model: "big", Effort: "hi",
		ExtraArgs: []string{"echo launched-$TEST_MODEL-$TEST_EFFORT; cat"}})
	path := "/api/agents/" + a.GetId()
	screenHas := func(what, text string) {
		t.Helper()
		var screen struct{ Screen string }
		waitFor(t, what, func() bool {
			e.webCall("GET", path+"/screen", "", &screen)
			return strings.Contains(screen.Screen, text)
		})
	}
	screenHas("the launch's model", "launched-big-hi")

	type modelReply struct{ Model *web.Model }
	chatModel := func(what, model, name, effort string, canSwitch bool) {
		t.Helper()
		var r modelReply
		if code := e.webCall("GET", path+"/chat", "", &r); code != 200 || r.Model == nil {
			t.Fatalf("%s: chat %d %+v", what, code, r)
		}
		m := r.Model
		if m.Model != model || m.Name != name || m.Effort != effort || m.Switch != canSwitch || len(m.Models) != 2 || len(m.Efforts) != 2 {
			t.Fatalf("%s: %+v, want %s %q %s switch=%v", what, m, model, name, effort, canSwitch)
		}
	}
	chatModel("launched, no transcript yet", "big", "", "hi", true)

	file := filepath.Join(testAdapter{}.TranscriptDir(""), a.GetSessionId()+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("hello\nmodel=big effort=lo\n"), 0o600)
	chatModel("from the transcript", "big", "big", "lo", true)

	var sw modelReply
	if code := e.webCall("POST", path+"/model", `{"model":"tiny","effort":""}`, &sw); code != 200 || sw.Model == nil ||
		sw.Model.Model != "tiny" || sw.Model.Effort != "" || !sw.Model.Switch {
		t.Fatalf("switch: %d %+v", code, sw.Model)
	}
	screenHas("the switch typed", "switched tiny")
	// The transcript does not say so yet; the switch holds until it does.
	chatModel("switched", "tiny", "", "", true)
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("model=big effort=hi\n")
	f.Close()
	chatModel("switched back in the terminal", "big", "big", "hi", true)

	var fail struct{ Error string }
	for body, want := range map[string]string{
		`{"model":"huge"}`:               "no model",
		`{"model":"tiny","effort":"lo"}`: "does not run at lo",
		`{"effort":"mid"}`:               "no effort",
		`{}`:                             "required",
	} {
		if code := e.webCall("POST", path+"/model", body, &fail); code != 400 || !strings.Contains(fail.Error, want) {
			t.Errorf("switch %s: %d %+v, want %q", body, code, fail, want)
		}
	}

	var ads struct{ Adapters []web.Adapter }
	e.webCall("GET", "/api/adapters", "", &ads)
	for _, ad := range ads.Adapters {
		if n := len(ad.Models); (ad.ID == "modeled") != (n == 2) {
			t.Errorf("adapter %s lists %d models", ad.ID, n)
		}
	}

	// Adapters without a choice take none, and cannot switch.
	plain := c.run(&fleetv1.RunAgentRequest{Root: "code", ExtraArgs: []string{"cat"}})
	if code := e.webCall("POST", "/api/agents/"+plain.GetId()+"/model", `{"model":"big"}`, &fail); code != 400 || !strings.Contains(fail.Error, "cannot switch") {
		t.Fatalf("switch a plain agent: %d %+v", code, fail)
	}
	var raw map[string]any
	if e.webCall("GET", "/api/agents/"+plain.GetId()+"/chat", "", &raw); raw["model"] != nil {
		t.Fatalf("a plain agent has a model: %v", raw["model"])
	}
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "code", Model: "big"}), fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	c.fails(runReq(&fleetv1.RunAgentRequest{Adapter: "modeled", Root: "code", Model: "tiny", Effort: "hi"}), fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)

	// A finished session shows its model, and cannot switch.
	if code := e.webCall("POST", path+"/stop", `{}`, nil); code != 200 {
		t.Fatalf("stop: %d", code)
	}
	chatModel("stopped", "big", "big", "hi", false)
	if code := e.webCall("POST", path+"/model", `{"model":"big"}`, &fail); code != 400 || !strings.Contains(fail.Error, "not running") {
		t.Fatalf("switch a stopped agent: %d %+v", code, fail)
	}
}
