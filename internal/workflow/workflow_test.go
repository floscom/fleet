package workflow

import (
	"reflect"
	"testing"
)

func TestTally(t *testing.T) {
	r := Run{Status: Running, Phases: []Phase{{Title: "Read"}, {Title: "Write"}}, Agents: []Agent{
		{ID: "a1", Phase: "Read", Status: Done, Tokens: 10, ToolUses: 1, UpdatedMs: 5},
		{ID: "a2", Phase: "Read", Status: Running, Tokens: 20, ToolUses: 2, UpdatedMs: 9},
		{ID: "a3", Phase: "Extra", Status: Failed, Tokens: 30, ToolUses: 3, UpdatedMs: 7},
	}}
	r.Tally()
	if r.Counts != (Counts{Running: 1, Done: 1, Failed: 1}) || r.Tokens != 60 || r.ToolUses != 6 || r.UpdatedMs != 9 {
		t.Fatalf("tally: %+v %d %d %d", r.Counts, r.Tokens, r.ToolUses, r.UpdatedMs)
	}
	// The running agent's phase is current, not the latest agent's; a phase
	// only an agent names is added after the declared ones.
	if r.Phase != "Read" || !reflect.DeepEqual(r.Phases, []Phase{{Title: "Read"}, {Title: "Write"}, {Title: "Extra"}}) {
		t.Fatalf("phases: %q %+v", r.Phase, r.Phases)
	}
	r.Tally() // again: nothing added twice
	if len(r.Phases) != 3 {
		t.Fatalf("phases after a second tally: %+v", r.Phases)
	}

	r.Stop()
	if r.Status != Stopped || r.Agents[1].Status != Stopped || r.Counts != (Counts{Done: 1, Failed: 1, Stopped: 1}) || r.Phase != "Extra" {
		t.Fatalf("stopped: %s %+v %+v %q", r.Status, r.Agents, r.Counts, r.Phase)
	}
	done := Run{Status: Completed, Agents: []Agent{{Status: Done}}}
	done.Stop()
	if done.Status != Completed {
		t.Fatalf("Stop changed an ended run: %s", done.Status)
	}
}
