package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/ask"
)

func TestHookWaitsForAnswer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{Adapters: adapter.NewRegistry(testAdapter{})}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	m.agents["a1"] = &agentRec{ID: "a1", Adapter: "test", State: stateWorking, OwnState: stateWorking}

	type result struct {
		out []byte
		err error
	}
	hook := func(ctx context.Context, event, payload string, wait bool) <-chan result {
		ch := make(chan result, 1)
		go func() {
			out, err := m.hook(ctx, &fleetv1.HookEvent{AgentId: "a1", Adapter: "test", Event: event, Payload: []byte(payload), Wait: wait})
			ch <- result{out, err}
		}()
		return ch
	}
	asks := func(n int) []ask.Pending {
		t.Helper()
		var ps []ask.Pending
		waitFor(t, "questions", func() bool { ps = m.asksOf("a1"); return len(ps) == n })
		return ps
	}
	done := func(ch <-chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("hook still waits")
			return result{}
		}
	}

	// Without --wait, or for a dialog that asks no questions, hooks return
	// at once.
	if r := done(hook(context.Background(), "ask", "/qcolor", false)); r.err != nil || r.out != nil {
		t.Fatalf("no wait: %+v", r)
	}
	if r := done(hook(context.Background(), "ask", "/Bash", true)); r.err != nil || r.out != nil {
		t.Fatalf("a permission: %+v", r)
	}
	if len(m.asksOf("a1")) != 0 {
		t.Fatal("questions without a waiting hook")
	}
	done(hook(context.Background(), "done", "/Bash", false))
	done(hook(context.Background(), "done", "/qcolor", false))

	// Answered on the dashboard.
	ch := hook(context.Background(), "ask", "sub1/qcolor", true)
	ps := asks(1)
	if len(ps[0].Questions) != 1 || ps[0].Questions[0].Question != "qcolor?" {
		t.Fatalf("asks: %+v", ps)
	}
	if got := m.agents["a1"].State; got != stateNeedsInput {
		t.Fatalf("state %v", got)
	}
	if err := m.answer("a1", ps[0].ID, ask.Answer{Answers: map[string][]string{"other?": {"yes"}}}); errorCodeOf(err) != codeInvalid {
		t.Fatalf("wrong question: %v", err)
	}
	if err := m.answer("a1", "nope", ask.Answer{}); errorCodeOf(err) != codeNotFound {
		t.Fatalf("unknown ask: %v", err)
	}
	ans := ask.Answer{Answers: map[string][]string{"qcolor?": {"teal"}}}
	if err := m.answer("a1", ps[0].ID, ans); err != nil {
		t.Fatal(err)
	}
	if r := done(ch); r.err != nil || string(r.out) != `{"answers":{"qcolor?":["teal"]}}` {
		t.Fatalf("answered: %+v %s", r, r.out)
	}
	if err := m.answer("a1", ps[0].ID, ans); errorCodeOf(err) != codeNotFound {
		t.Fatalf("answered twice: %v", err)
	}
	done(hook(context.Background(), "done", "sub1/qcolor", false))

	// Answered in the terminal: the call's next hook closes the dialog and
	// the waiting hook ends with nothing to print. Several questions show
	// in the order their dialogs opened.
	ch1 := hook(context.Background(), "ask", "/qsize", true)
	asks(1)
	ch2 := hook(context.Background(), "ask", "/qshape", true)
	ps = asks(2)
	if ps[0].Questions[0].Question != "qsize?" || ps[1].Questions[0].Question != "qshape?" {
		t.Fatalf("order: %+v", ps)
	}
	done(hook(context.Background(), "done", "/qsize", false))
	if r := done(ch1); r.err != nil || r.out != nil {
		t.Fatalf("answered in the terminal: %+v", r)
	}
	if ps := asks(1); ps[0].Questions[0].Question != "qshape?" {
		t.Fatalf("left: %+v", ps)
	}

	// The hook went away (its connection closed).
	ctx, cancel := context.WithCancel(context.Background())
	ch3 := hook(ctx, "ask", "/qshape", true) // the same dialog, asked again
	asks(2)
	cancel()
	if r := done(ch3); r.out != nil {
		t.Fatalf("cancelled: %+v", r)
	}
	if len(m.asksOf("a1")) != 1 {
		t.Fatalf("asks after cancel: %+v", m.asksOf("a1"))
	}

	// The agent ended.
	m.mu.Lock()
	m.agents["a1"].State = fleetv1.AgentState_AGENT_STATE_EXITED
	m.mu.Unlock()
	if r := done(ch2); r.out != nil {
		t.Fatalf("agent ended: %+v", r)
	}
}

// errorCodeOf is the protocol code of err, codeInternal for others.
func errorCodeOf(err error) fleetv1.ErrorCode {
	if err == nil {
		return 0
	}
	code, _ := errorCode(err)
	return code
}
