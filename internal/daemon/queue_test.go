package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/web"
	"fleet/internal/workflow"
)

// Queued messages go in one by one, each once the agent is done with its
// turn: idle, no workflow running in its background, the queue not paused
// (as Stop does).
func TestQueue(t *testing.T) {
	oldInterval, oldSettle, oldWait := queueInterval, queueSettle, interruptWait
	queueInterval, queueSettle, interruptWait = 50*time.Millisecond, 150*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { queueInterval, queueSettle, interruptWait = oldInterval, oldSettle, oldWait })

	e := newWebEnv(t)
	c := e.dialUnix()
	dir := testAdapter{}.TranscriptDir("")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript, got := filepath.Join(dir, "s.jsonl"), filepath.Join(e.t.TempDir(), "got")
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent logs every line typed into it.
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", ExtraArgs: []string{
		`while IFS= read -r l; do printf '%s\n' "$l" >>` + got + `; done`,
	}})
	hook := func(event, payload string) {
		t.Helper()
		c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
			AgentId: a.GetId(), Adapter: "test", Event: event, Payload: []byte(payload),
		}}})
	}
	edit := func(r *fleetv1.EditQueueRequest) *fleetv1.Agent {
		t.Helper()
		r.Agent = a.GetName()
		return c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_EditQueue{EditQueue: r}}).GetEditQueue().GetAgent()
	}
	add := func(text string) *fleetv1.Agent {
		return edit(&fleetv1.EditQueueRequest{Op: &fleetv1.EditQueueRequest_Add{Add: text}})
	}
	lines := func() string {
		b, _ := os.ReadFile(got)
		return string(b)
	}
	// quiet checks that nothing is typed in for a few settle times.
	quiet := func(what string) {
		t.Helper()
		before := lines()
		time.Sleep(3 * queueSettle)
		if s := lines(); s != before {
			t.Fatalf("%s: %q", what, s)
		}
	}
	hook("transcript", transcript)
	hook("working", "")
	c.waitAgent(a.GetId(), "working", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })

	add("  first  ")
	q := add("second").GetQueue()
	if len(q) != 2 || q[0].GetText() != "first" || q[1].GetText() != "second" || q[0].GetId() == q[1].GetId() {
		t.Fatalf("queue: %v", q)
	}
	drop := add("dropped").GetQueue()[2].GetId()
	if q := edit(&fleetv1.EditQueueRequest{Op: &fleetv1.EditQueueRequest_Remove{Remove: drop}}).GetQueue(); len(q) != 2 {
		t.Fatalf("after remove: %v", q)
	}
	bad := &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_EditQueue{EditQueue: &fleetv1.EditQueueRequest{Agent: a.GetId(), Op: &fleetv1.EditQueueRequest_Add{Add: " \n"}}}}
	c.fails(bad, fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	bad.GetEditQueue().Op = &fleetv1.EditQueueRequest_Remove{Remove: drop}
	c.fails(bad, fleetv1.ErrorCode_ERROR_CODE_NOT_FOUND)
	quiet("sent while working")

	// Done: the first goes in, and the queue waits for that turn.
	hook("idle", "")
	waitFor(t, "the first message", func() bool { return lines() == "first\n" })
	if q := c.agent(a.GetId()).GetQueue(); len(q) != 1 || q[0].GetText() != "second" {
		t.Fatalf("after sending: %v", q)
	}
	quiet("sent before the first turn showed")

	// Stop pauses the queue.
	hook("working", "")
	c.waitAgent(a.GetId(), "working", func(a *fleetv1.Agent) bool { return a.GetState() == stateWorking })
	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_InterruptAgent{InterruptAgent: &fleetv1.InterruptAgentRequest{Agent: a.GetId()}}})
	hook("idle", "")
	if !c.waitAgent(a.GetId(), "idle", func(a *fleetv1.Agent) bool { return a.GetState() == stateIdle }).GetQueuePaused() {
		t.Fatal("Stop did not pause the queue")
	}
	quiet("sent while paused")

	// Resumed, but a workflow runs in the background.
	runs := strings.TrimSuffix(transcript, ".jsonl") + ".runs"
	b, _ := json.Marshal([]workflow.Run{{ID: "r1", Status: workflow.Running, StartedMs: time.Now().UnixMilli()}})
	if err := os.WriteFile(runs, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var res struct{ Agent web.Agent }
	if code := e.webCall("POST", "/api/agents/"+a.GetId()+"/queue", `{"paused":false}`, &res); code != 200 || res.Agent.QueuePaused || len(res.Agent.Queue) != 1 {
		t.Fatalf("resume: %d %+v", code, res)
	}
	quiet("sent while a workflow runs")
	if err := os.Remove(runs); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second message", func() bool { return strings.HasSuffix(lines(), "second\n") })
	if a := c.agent(a.GetId()); len(a.GetQueue()) != 0 || a.GetQueuePaused() {
		t.Fatalf("after the last: %v", a)
	}

	// Pausing an empty queue holds nothing.
	if a := edit(&fleetv1.EditQueueRequest{Op: &fleetv1.EditQueueRequest_Paused{Paused: true}}); a.GetQueuePaused() {
		t.Fatal("an empty queue paused")
	}
}
