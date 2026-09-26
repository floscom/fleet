package daemon

import (
	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

// hook routes an event from `fleet hook` to the agent's adapter and applies
// the resulting state update. Updates for finished agents are ignored, and
// hooks can never end an agent: tmux is the source of truth for exits.
func (m *manager) hook(ev *fleetv1.HookEvent) error {
	m.mu.Lock()
	a, ok := m.agents[ev.GetAgentId()]
	var adapterID string
	if ok {
		adapterID = a.Adapter
	}
	m.mu.Unlock()
	if !ok {
		return errf(codeNotFound, "no agent %q", ev.GetAgentId())
	}
	if ev.GetAdapter() != "" && ev.GetAdapter() != adapterID {
		return errf(codeInvalid, "agent %s runs adapter %q, not %q", ev.GetAgentId(), adapterID, ev.GetAdapter())
	}
	ad, ok := m.d.opts.Adapters.Get(adapterID)
	if !ok {
		return errf(codeNotFound, "adapter %q is not registered", adapterID)
	}
	upd, ok := ad.HandleHook(adapter.HookEvent{AgentID: ev.GetAgentId(), Event: ev.GetEvent(), Payload: ev.GetPayload()})
	if !ok {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !a.live() {
		return nil
	}
	changed := false
	if upd.SessionID != "" && upd.SessionID != a.SessionID {
		a.SessionID = upd.SessionID
		changed = true
	}
	if upd.State > stateStarting && isLive(upd.State) && (upd.State != a.State || upd.Detail != a.StateDetail) {
		a.State, a.StateDetail, a.ScreenPrompt = upd.State, upd.Detail, false
		changed = true
	}
	if changed {
		m.changedLocked(a)
	}
	return nil
}
