package daemon

import (
	"context"
	"net/http"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/web"
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
		out = append(out, web.Adapter{ID: a.Id, Name: a.DisplayName, Available: a.Available})
	}
	return out
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
	case codeBusy:
		return &web.Error{Status: http.StatusConflict, Msg: msg}
	case codeUnavailable:
		return &web.Error{Status: http.StatusServiceUnavailable, Msg: msg}
	}
	return err
}
