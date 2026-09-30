package daemon

import (
	"context"
	"net/http"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/web"
	"fleet/internal/workflow"
)

// webSource shows the daemon on the web dashboard (web.Source).
type webSource struct {
	d *daemon
	// listen is the effective TLS listen address or "off"; mdns reports
	// whether the daemon advertises itself.
	listen string
	mdns   bool
}

func (s *webSource) Info() web.Info {
	i := s.d.info()
	return web.Info{
		ID: i.ServerId, Name: i.ServerName, Version: i.DaemonVersion, Hostname: i.Hostname,
		OS: i.Os, Arch: i.Arch, StartedAtMs: i.StartedAtMs, Listen: s.listen, MDNS: s.mdns,
	}
}

func (s *webSource) SubscribeAgents() (<-chan *fleetv1.Event, func()) {
	sub := s.d.agents.subscribe()
	return sub.ch, func() { s.d.agents.unsubscribe(sub) }
}

func (s *webSource) Devices() []web.Device {
	devs := s.d.listDevices()
	out := make([]web.Device, 0, len(devs))
	for _, dev := range devs {
		out = append(out, web.Device{
			ID: dev.Id, Name: dev.Name, Platform: dev.Platform,
			PairedAtMs: dev.PairedAtMs, LastSeenAtMs: dev.LastSeenAtMs, Connected: dev.Connected,
		})
	}
	return out
}

func (s *webSource) Roots() []*fleetv1.Root { return s.d.roots() }

func (s *webSource) AddRoot(req *fleetv1.AddRootRequest) (*fleetv1.Root, error) {
	r, err := s.d.addRoot(req)
	return r, webError(err)
}

func (s *webSource) RemoveRoot(name string) error {
	return webError(s.d.removeRoot(name))
}

func (s *webSource) Adapters(ctx context.Context) []web.Adapter {
	list := s.d.listAdapters(ctx).GetAdapters()
	out := make([]web.Adapter, 0, len(list))
	for _, a := range list {
		wa := web.Adapter{
			ID: a.Id, Name: a.DisplayName, Available: a.Available,
			Models: []web.ModelChoice{}, Efforts: []web.EffortChoice{},
		}
		for _, m := range a.Models {
			wa.Models = append(wa.Models, web.ModelChoice{ID: m.Id, Label: m.Label, Efforts: nonNilStrings(m.Efforts)})
		}
		for _, e := range a.Efforts {
			wa.Efforts = append(wa.Efforts, web.EffortChoice{ID: e.Id, Label: e.Label})
		}
		out = append(out, wa)
	}
	return out
}

// webModel shows a session's model on the dashboard.
func webModel(info ModelInfo) *web.Model {
	m := &web.Model{
		Model: info.Model, Name: info.Name, Effort: info.Effort, Switch: info.Switch,
		Models: []web.ModelChoice{}, Efforts: []web.EffortChoice{},
	}
	for _, c := range info.Choices.Models {
		m.Models = append(m.Models, web.ModelChoice{ID: c.ID, Label: c.Label, Efforts: nonNilStrings(c.Efforts)})
	}
	for _, e := range info.Choices.Efforts {
		m.Efforts = append(m.Efforts, web.EffortChoice{ID: e.ID, Label: e.Label})
	}
	return m
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *webSource) SwitchModel(ctx context.Context, agent, model, effort string) (*web.Model, error) {
	info, err := s.d.agents.switchModel(ctx, agent, model, effort)
	if err != nil {
		return nil, webError(err)
	}
	return webModel(info), nil
}

func (s *webSource) Agents() []*fleetv1.Agent { return s.d.agents.list(true) }

func (s *webSource) RunAgent(ctx context.Context, req *fleetv1.RunAgentRequest) (*fleetv1.Agent, error) {
	a, err := s.d.agents.run(ctx, req)
	return a, webError(err)
}

func (s *webSource) StopAgent(ctx context.Context, req *fleetv1.KillAgentRequest) (*fleetv1.KillAgentResponse, error) {
	r, err := s.d.agents.kill(ctx, req)
	return r, webError(err)
}

func (s *webSource) SendInput(ctx context.Context, agent, text string, submit bool, keys []string) (bool, error) {
	held, err := s.d.agents.sendInput(ctx, agent, text, submit, true, keys)
	return held, webError(err)
}

func (s *webSource) Screen(ctx context.Context, agent string) (string, error) {
	screen, err := s.d.agents.screen(ctx, agent)
	return screen, webError(err)
}

func (s *webSource) Chat(agent string) (web.Chat, error) {
	a, path, parse, err := s.d.agents.chat(agent)
	if err != nil {
		return web.Chat{}, webError(err)
	}
	c := web.Chat{Agent: a, Path: path, Parse: parse}
	if info, ok, err := s.d.agents.model(a.GetId()); err == nil && ok {
		c.Model = webModel(info)
	}
	return c, nil
}

func (s *webSource) Workflows(agent string) (*fleetv1.Agent, []workflow.Run, error) {
	a, runs, err := s.d.agents.workflows(agent)
	return a, runs, webError(err)
}

func (s *webSource) WorkflowChat(agent, run, sub string) (web.Chat, error) {
	a, path, parse, err := s.d.agents.workflowChat(agent, run, sub)
	if err != nil {
		return web.Chat{}, webError(err)
	}
	return web.Chat{Agent: a, Path: path, Parse: parse}, nil
}

// webError turns a protocol error into a *web.Error the page may show;
// anything else stays internal.
func webError(err error) error {
	if err == nil {
		return nil
	}
	code, msg := errorCode(err)
	switch code {
	case codeNotFound:
		return &web.Error{Status: http.StatusNotFound, Msg: msg}
	case codeInvalid, codeOutside, codeExists:
		return &web.Error{Status: http.StatusBadRequest, Msg: msg}
	case codeDenied:
		return &web.Error{Status: http.StatusForbidden, Msg: msg}
	case codeUnavailable:
		return &web.Error{Status: http.StatusServiceUnavailable, Msg: msg}
	}
	return err
}
