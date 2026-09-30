package daemon

import (
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

func TestApplyHookDialogs(t *testing.T) {
	const (
		working = fleetv1.AgentState_AGENT_STATE_WORKING
		idle    = fleetv1.AgentState_AGENT_STATE_IDLE
		needs   = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
	)
	type step struct {
		by, call string // by "" = the agent itself
		state    fleetv1.AgentState
		detail   string
		// wantState and wantDetail are the agent's state after the step.
		wantState  fleetv1.AgentState
		wantDetail string
	}
	tests := []struct {
		name  string
		start *agentRec
		steps []step
	}{
		{"subagent tools do not answer the agent's question", &agentRec{State: working, OwnState: working}, []step{
			{"", "q", needs, "Claude asks: Name?", needs, "Claude asks: Name?"},
			{"sub1", "Bash: ls", working, "", needs, "Claude asks: Name?"},
			{"sub1", "Bash: ls", working, "", needs, "Claude asks: Name?"},
			{"", "q", working, "", working, ""},
		}},
		{"a parallel call does not answer the question", &agentRec{State: working, OwnState: working}, []step{
			{"", "q", needs, "Claude asks: Color?", needs, "Claude asks: Color?"},
			{"", "Agent: research", working, "", needs, "Claude asks: Color?"},
			{"", "", idle, "", idle, ""}, // the turn ended (or was interrupted)
		}},
		{"a dialog without a call closes on any news", &agentRec{State: working, OwnState: working}, []step{
			{"", "", needs, "Claude needs your permission", needs, "Claude needs your permission"},
			{"", "Bash: ls", working, "", working, ""},
		}},
		{"the agent does not answer a subagent's permission", &agentRec{State: working, OwnState: working}, []step{
			{"sub1", "", needs, "Bash: ls /", needs, "Bash: ls /"},
			{"", "", working, "", needs, "Bash: ls /"},
			{"", "", idle, "", needs, "Bash: ls /"},
			{"sub1", "", working, "", idle, ""}, // e.g. SubagentStop after a refusal
		}},
		{"dialogs show in the order they opened", &agentRec{State: working, OwnState: working}, []step{
			{"", "", needs, "Claude asks: Color?", needs, "Claude asks: Color?"},
			{"sub1", "", needs, "Bash: ls /", needs, "Claude asks: Color?"},
			{"", "", needs, "Claude asks: Size?", needs, "Claude asks: Size?"},
			{"", "", working, "", needs, "Bash: ls /"},
			{"sub2", "", working, "", needs, "Bash: ls /"},
			{"sub1", "", working, "", working, ""},
		}},
		{"a record from before dialogs were tracked", &agentRec{State: needs, StateDetail: "Bash: rm"}, []step{
			{"sub1", "", working, "", needs, "Bash: rm"},
			{"", "", working, "", working, ""},
		}},
		{"hooks take over from a dialog on the screen", &agentRec{State: needs, StateDetail: "trust?", ScreenPrompt: true}, []step{
			{"", "", idle, "", idle, ""},
		}},
	}
	for _, tt := range tests {
		a := tt.start
		for i, s := range tt.steps {
			a.applyHook(adapter.StateUpdate{State: s.state, Detail: s.detail, Subagent: s.by, Call: s.call})
			if a.State != s.wantState || a.StateDetail != s.wantDetail {
				t.Errorf("%s: step %d: %v %q, want %v %q", tt.name, i, a.State, a.StateDetail, s.wantState, s.wantDetail)
			}
			if a.ScreenPrompt {
				t.Errorf("%s: step %d: ScreenPrompt still set", tt.name, i)
			}
		}
	}
}
