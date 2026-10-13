package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

func TestSetNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{Adapters: adapter.NewRegistry(testAdapter{})}}
	m := &manager{d: d, path: filepath.Join(dir, "agents.json"), agents: map[string]*agentRec{}, subs: map[*subscriber]struct{}{}}
	d.agents = m
	m.agents["a1"] = &agentRec{ID: "a1", Name: "claude-x-1", Adapter: "test", State: fleetv1.AgentState_AGENT_STATE_EXITED, UpdatedAtMs: 1000}
	sub := m.subscribe()
	defer m.unsubscribe(sub)
	for len(sub.ch) > 0 {
		<-sub.ch // the snapshot
	}

	a, err := m.setNote("claude-x-1", "  customer is testing, keep it open \n")
	if err != nil || a.GetNote() != "customer is testing, keep it open" {
		t.Fatalf("set: %v %q", err, a.GetNote())
	}
	if a.GetUpdatedAtMs() != 1000 {
		t.Fatalf("a note moved updated_at_ms to %d", a.GetUpdatedAtMs())
	}
	if ev := <-sub.ch; ev.GetAgentUpserted().GetNote() != a.GetNote() {
		t.Fatalf("event: %v", ev)
	}
	b, _ := os.ReadFile(m.path)
	var saved []agentRec
	if err := json.Unmarshal(b, &saved); err != nil || len(saved) != 1 || saved[0].Note != a.GetNote() {
		t.Fatalf("saved: %v %s", err, b)
	}

	if _, err := m.setNote("a1", a.GetNote()); err != nil || len(sub.ch) != 0 {
		t.Fatalf("the same note again: %v, %d events", err, len(sub.ch))
	}
	if _, err := m.setNote("a1", strings.Repeat("é", maxNote+1)); err == nil {
		t.Fatal("a note too long was taken")
	}
	if _, err := m.setNote("a1", strings.Repeat("é", maxNote)); err != nil {
		t.Fatalf("a note of %d characters: %v", maxNote, err)
	}
	if a, err := m.setNote("a1", " "); err != nil || a.GetNote() != "" || m.agents["a1"].Note != "" {
		t.Fatalf("clear: %v %q", err, a.GetNote())
	}
	if _, err := m.setNote("nope", "x"); err == nil {
		t.Fatal("note on an unknown agent")
	}
}
