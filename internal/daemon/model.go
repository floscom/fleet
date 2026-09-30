package daemon

// The model and reasoning effort agents run at (adapter.Modeler): chosen at
// launch, followed in the session's transcript, and switched in a running
// session from the dashboard (adapter.ModelSwitcher).

import (
	"context"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/tmux"
	"fleet/internal/transcript"
)

// switchTimeout bounds switching a session's model: the adapter types into
// its terminal and waits for each step to show.
const switchTimeout = 20 * time.Second

// modelNameRe is what a model may look like: --model and -m take aliases,
// full names and provider ids ("claude-opus-5-5[1m]", "us.anthropic.…").
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// checkModel validates the model and effort an agent of ad is to run at.
func checkModel(ad adapter.Adapter, model, effort string) error {
	if model == "" && effort == "" {
		return nil
	}
	md, ok := ad.(adapter.Modeler)
	if !ok {
		return errf(codeInvalid, "%s offers no choice of model or effort", ad.DisplayName())
	}
	ms := md.Models()
	if model != "" && !modelNameRe.MatchString(model) {
		return errf(codeInvalid, "invalid model %q", model)
	}
	if effort != "" && !ms.HasEffort(effort) {
		return errf(codeInvalid, "%s has no effort %q", ad.DisplayName(), effort)
	}
	if mod, ok := ms.Find(model); ok && effort != "" && !slices.Contains(mod.Efforts, effort) {
		return errf(codeInvalid, "%s does not run at %s effort", mod.Label, effort)
	}
	return nil
}

// ModelInfo is the model and effort of an agent's session and what it can
// switch to.
type ModelInfo struct {
	adapter.SessionModel
	Choices adapter.Models
	// Switch is set when the running session can be switched from here.
	Switch bool
}

// modelCursor follows a session's transcript for its model and effort.
type modelCursor struct {
	mu   sync.Mutex
	path string
	off  int64
	m    adapter.SessionModel
}

// follow reads what was appended to the transcript at path ("" while there
// is none) since the last call. A new or replaced transcript starts over
// from initial, what the agent was launched with or last switched to.
func (c *modelCursor) follow(md adapter.Modeler, path string, initial adapter.SessionModel) adapter.SessionModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.followLocked(md, path, initial)
	return c.m
}

func (c *modelCursor) followLocked(md adapter.Modeler, path string, initial adapter.SessionModel) {
	if path == "" || c.path != path {
		c.path, c.off, c.m = path, 0, initial
	}
	if path == "" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Size() < c.off {
		c.off, c.m = 0, initial
	}
	visit := func(line []byte) []transcript.Entry {
		md.ModelLine(line, &c.m)
		return nil
	}
	for c.off < fi.Size() {
		p, err := transcript.Forward(path, c.off, visit)
		if err != nil || p.End == c.off {
			return // unreadable, or only a line still being written
		}
		c.off = p.End
		if !p.More {
			return
		}
	}
}

// launched is the session model an agent starts from: what it was
// launched with or last switched to.
func launched(md adapter.Modeler, a *agentRec) adapter.SessionModel {
	m := adapter.SessionModel{Effort: a.Effort}
	if _, ok := md.Models().Find(a.Model); ok {
		m.Model = a.Model
	} else {
		m.Name = a.Model
	}
	return m
}

// cursor returns the model cursor of agent id.
func (m *manager) cursor(id string) *modelCursor {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	c := m.models[id]
	if c == nil {
		if m.models == nil {
			m.models = map[string]*modelCursor{}
		}
		c = &modelCursor{}
		m.models[id] = c
	}
	return c
}

// dropCursor forgets the model cursor of agent id.
func (m *manager) dropCursor(id string) {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	delete(m.models, id)
}

// model returns the model and effort of an agent's session; ok is false
// when its adapter offers no choice of model.
func (m *manager) model(ref string) (info ModelInfo, ok bool, err error) {
	s, err := m.session(ref)
	if err != nil {
		return ModelInfo{}, false, err
	}
	ad, _ := m.d.opts.Adapters.Get(s.agent.GetAdapter())
	md, ok := ad.(adapter.Modeler)
	if !ok {
		return ModelInfo{}, false, nil
	}
	m.mu.Lock()
	a, err := m.find(s.agent.GetId())
	var initial adapter.SessionModel
	live := false
	if err == nil {
		initial, live = launched(md, a), a.live()
	}
	m.mu.Unlock()
	if err != nil {
		return ModelInfo{}, false, err
	}
	_, canSwitch := ad.(adapter.ModelSwitcher)
	return ModelInfo{
		SessionModel: m.cursor(s.agent.GetId()).follow(md, s.path, initial),
		Choices:      md.Models(),
		Switch:       canSwitch && live,
	}, true, nil
}

// switchModel switches a live agent's session to model and/or effort ("":
// keep), for that session only.
func (m *manager) switchModel(ctx context.Context, ref, model, effort string) (ModelInfo, error) {
	session, adapterID, err := m.liveSession(ref)
	if err != nil {
		return ModelInfo{}, err
	}
	ad, _ := m.d.opts.Adapters.Get(adapterID)
	md, ok := ad.(adapter.Modeler)
	sw, canSwitch := ad.(adapter.ModelSwitcher)
	if !ok || !canSwitch {
		name := adapterID
		if ad != nil {
			name = ad.DisplayName()
		}
		return ModelInfo{}, errf(codeInvalid, "%s cannot switch the model of a running session", name)
	}
	if model == "" && effort == "" {
		return ModelInfo{}, errf(codeInvalid, "model or effort is required")
	}
	ms := md.Models()
	mod, known := ms.Find(model)
	if model != "" && !known {
		return ModelInfo{}, errf(codeInvalid, "%s has no model %q", ad.DisplayName(), model)
	}
	if err := checkModel(ad, model, effort); err != nil {
		return ModelInfo{}, err
	}

	s, err := m.session(ref)
	if err != nil {
		return ModelInfo{}, err
	}
	id := s.agent.GetId()
	// One switch at a time per agent: each drives the same terminal.
	m.modelMu.Lock()
	if m.switching[id] {
		m.modelMu.Unlock()
		return ModelInfo{}, errf(codeInvalid, "already switching the model of %s", s.agent.GetName())
	}
	if m.switching == nil {
		m.switching = map[string]bool{}
	}
	m.switching[id] = true
	m.modelMu.Unlock()
	defer func() {
		m.modelMu.Lock()
		delete(m.switching, id)
		m.modelMu.Unlock()
	}()

	sctx, cancel := context.WithTimeout(ctx, switchTimeout)
	defer cancel()
	if err := sw.SwitchModel(sctx, terminal{m.d.tmux, session}, model, effort); err != nil {
		return ModelInfo{}, errf(codeUnavailable, "%v", err)
	}

	// Remember the switch: the transcript may not record it (or only with
	// the next reply), and a transcript that appears later starts from it.
	m.mu.Lock()
	var initial adapter.SessionModel
	if a, err := m.find(id); err == nil {
		if model != "" {
			a.Model = model
		}
		if effort != "" {
			a.Effort = effort
		}
		if known && len(mod.Efforts) == 0 {
			a.Effort = ""
		}
		initial = launched(md, a)
		m.saveLocked()
	}
	m.mu.Unlock()
	c := m.cursor(id)
	c.mu.Lock()
	// What the transcript holds so far predates the switch.
	c.followLocked(md, s.path, initial)
	if model != "" {
		c.m.Model, c.m.Name = model, ""
	}
	if effort != "" {
		c.m.Effort = effort
	}
	if known && len(mod.Efforts) == 0 {
		c.m.Effort = ""
	}
	cur := c.m
	c.mu.Unlock()
	return ModelInfo{SessionModel: cur, Choices: ms, Switch: true}, nil
}

// terminal is a live agent's tmux session, as adapters drive it.
type terminal struct {
	t       *tmux.Tmux
	session string
}

func (t terminal) Screen(ctx context.Context) (string, error) {
	sctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()
	return t.t.Screen(sctx, t.session)
}

func (t terminal) Type(ctx context.Context, text string, submit bool) error {
	return t.t.SendText(ctx, t.session, text, submit)
}

func (t terminal) Press(ctx context.Context, keys ...string) error {
	return t.t.SendKeys(ctx, t.session, keys...)
}
