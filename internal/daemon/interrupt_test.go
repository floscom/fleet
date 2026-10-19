package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/web"
)

// Esc interrupts a turn, and the agent is idle once its transcript says so:
// no hook reports a turn interrupted while the model writes.
func TestInterrupt(t *testing.T) {
	old := interruptWait
	interruptWait = 500 * time.Millisecond
	t.Cleanup(func() { interruptWait = old })

	e := newWebEnv(t)
	c := e.dialUnix()
	dir := testAdapter{}.TranscriptDir("")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log, quiet := filepath.Join(dir, "s.jsonl"), filepath.Join(dir, "quiet.jsonl")
	for _, p := range []string{log, quiet} {
		if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The agent logs each Esc it reads as an interrupted turn.
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", ExtraArgs: []string{
		`stty -icanon -echo min 1; esc=$(printf '\033'); while :; do c=$(dd bs=1 count=1 2>/dev/null); [ "$c" = "$esc" ] && echo interrupted >>` + log + `; done`,
	}})
	hook := func(event, payload string) {
		t.Helper()
		c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
			AgentId: a.GetId(), Adapter: "test", Event: event, Payload: []byte(payload),
		}}})
	}
	interrupt := &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_InterruptAgent{InterruptAgent: &fleetv1.InterruptAgentRequest{Agent: a.GetName()}}}
	hook("transcript", log)

	// Not working: Esc twice at an idle prompt opens Claude's rewind menu.
	if e := c.fails(interrupt, fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT); e.GetMessage() != "agent "+a.GetId()+" is not working" {
		t.Fatalf("interrupt before work: %s", e.GetMessage())
	}

	hook("working", "")
	c.waitAgent(a.GetId(), "working", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })
	r := c.ok(interrupt).GetInterruptAgent()
	if !r.GetInterrupted() || r.GetAgent().GetState() != stateIdle {
		t.Fatalf("interrupt: %v", r)
	}

	// A transcript that records nothing: Esc was pressed, the agent works on.
	hook("working", "")
	hook("transcript", quiet)
	c.waitAgent(a.GetId(), "working again", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })
	var res struct {
		Agent       web.Agent
		Interrupted bool
	}
	if code := e.webCall("POST", "/api/agents/"+a.GetId()+"/interrupt", "", &res); code != 200 || res.Interrupted || res.Agent.State != "working" {
		t.Fatalf("interrupt without a record: %d %+v", code, res)
	}
	waitFor(t, "the second Esc read", func() bool {
		b, _ := os.ReadFile(log)
		return string(b) == "hello\ninterrupted\ninterrupted\n"
	})
}
