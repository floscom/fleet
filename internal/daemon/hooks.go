package daemon

import (
	"context"
	"slices"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/ask"
)

// hook routes an event from `fleet hook` to the agent's adapter and applies
// the resulting state update. Updates for finished agents are ignored, and
// hooks can never end an agent: tmux is the source of truth for exits.
//
// A waiting hook (`fleet hook --wait`) whose dialog asks questions the
// dashboard can answer (adapter.Asker) returns only once they are
// answered or the dialog is gone, with what the hook prints.
func (m *manager) hook(ctx context.Context, ev *fleetv1.HookEvent) ([]byte, error) {
	m.mu.Lock()
	a, ok := m.agents[ev.GetAgentId()]
	var adapterID string
	if ok {
		adapterID = a.Adapter
	}
	m.mu.Unlock()
	if !ok {
		return nil, errf(codeNotFound, "no agent %q", ev.GetAgentId())
	}
	if ev.GetAdapter() != "" && ev.GetAdapter() != adapterID {
		return nil, errf(codeInvalid, "agent %s runs adapter %q, not %q", ev.GetAgentId(), adapterID, ev.GetAdapter())
	}
	ad, ok := m.d.opts.Adapters.Get(adapterID)
	if !ok {
		return nil, errf(codeNotFound, "adapter %q is not registered", adapterID)
	}
	hev := adapter.HookEvent{AgentID: ev.GetAgentId(), Event: ev.GetEvent(), Payload: ev.GetPayload()}
	upd, ok := ad.HandleHook(hev)
	if !ok {
		return nil, nil
	}
	var qs []ask.Question
	asker, _ := ad.(adapter.Asker)
	if ev.GetWait() && asker != nil && upd.State == stateNeedsInput {
		qs, _ = asker.Questions(hev)
	}

	// The question is recorded with the state change, under the same lock:
	// whoever sees the agent's dialog sees its question too.
	m.mu.Lock()
	m.applyHookLocked(a, ad, upd)
	var p *pendingAsk
	if len(qs) > 0 && a.live() {
		p = m.addAskLocked(a, upd.Subagent, upd.Call, qs, hev, asker)
	}
	m.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	return m.awaitAnswer(ctx, p), nil
}

// applyHookLocked applies a hook's state update to a live agent.
func (m *manager) applyHookLocked(a *agentRec, ad adapter.Adapter, upd adapter.StateUpdate) {
	if !a.live() {
		return
	}
	changed := false
	if upd.SessionID != "" && upd.SessionID != a.SessionID {
		a.SessionID, a.Dialogs = upd.SessionID, nil
		changed = true
	}
	if upd.State > stateStarting && isLive(upd.State) && a.applyHook(upd) {
		changed = true
	}
	// The transcript is not part of the Agent message: saved, not broadcast.
	saved := false
	if upd.Transcript != "" {
		if p := transcriptFile(ad, a, upd.Transcript); p != "" && p != a.Transcript {
			a.Transcript = p
			saved = true
		}
	}
	switch {
	case changed:
		m.changedLocked(a)
	case saved:
		m.saveLocked()
	}
}

// dialog is a dialog a hook reported open.
type dialog struct {
	// By is the subagent that opened it, "" for the agent itself, and Call
	// the tool call it asks about ("" if unknown).
	By     string `json:"by,omitempty"`
	Call   string `json:"call,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// applyHook folds a hook's state update into the agent and reports whether
// its state changed. NEEDS_INPUT opens a dialog for the sender and its
// call; any other update from the sender closes the dialog of that call,
// or all of theirs when it is about no call (a turn or subagent ended).
// Only the agent's own updates set the state it returns to: tool calls
// go on while a dialog waits for the user (in parallel, or in background
// subagents), and must not make the agent look busy.
func (a *agentRec) applyHook(upd adapter.StateUpdate) bool {
	if a.OwnState == 0 && a.State != stateNeedsInput {
		// A record from before dialogs were tracked.
		a.OwnState, a.OwnDetail = a.State, a.StateDetail
	}
	if upd.State == stateNeedsInput {
		i := slices.IndexFunc(a.Dialogs, func(d dialog) bool { return d.By == upd.Subagent && d.Call == upd.Call })
		if i >= 0 {
			a.Dialogs[i].Detail = upd.Detail
		} else {
			a.Dialogs = append(a.Dialogs, dialog{By: upd.Subagent, Call: upd.Call, Detail: upd.Detail})
		}
	} else {
		a.Dialogs = slices.DeleteFunc(a.Dialogs, func(d dialog) bool {
			return d.By == upd.Subagent && (upd.Call == "" || d.Call == "" || d.Call == upd.Call)
		})
		if upd.Subagent == "" {
			a.OwnState, a.OwnDetail = upd.State, upd.Detail
		}
	}
	a.dialogGone = 0
	return a.showDialogs()
}

// showDialogs sets the agent's state from its dialogs: NEEDS_INPUT for the
// oldest (the CLI shows them one at a time, in order), else its own state.
// It reports whether the state changed.
func (a *agentRec) showDialogs() bool {
	state, detail := a.OwnState, a.OwnDetail
	if len(a.Dialogs) > 0 {
		state, detail = stateNeedsInput, a.Dialogs[0].Detail
	}
	if state <= stateStarting || (state == a.State && detail == a.StateDetail && !a.ScreenPrompt) {
		return false
	}
	// Hooks take over from a dialog seen on the screen.
	a.State, a.StateDetail, a.ScreenPrompt = state, detail, false
	return true
}
