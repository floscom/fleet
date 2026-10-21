package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/workflow"
)

// awakeManager is a manager of the given agents, without a daemon around it.
func awakeManager(t *testing.T, cfg *config.Config, agents ...*agentRec) *manager {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	d := &daemon{
		opts: Options{Adapters: adapter.NewRegistry(testAdapter{})},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:  cfg,
	}
	m := &manager{d: d, agents: map[string]*agentRec{}}
	for _, a := range agents {
		m.agents[a.ID] = a
	}
	return m
}

func TestAwakeReason(t *testing.T) {
	now := time.Now()
	resumeAt := func(d time.Duration) *fleetv1.UsageLimit {
		return &fleetv1.UsageLimit{ResumeAtMs: now.Add(d).UnixMilli()}
	}
	idle := fleetv1.AgentState_AGENT_STATE_IDLE
	for _, tc := range []struct {
		name string
		a    agentRec
		want string
	}{
		{"working", agentRec{State: fleetv1.AgentState_AGENT_STATE_WORKING}, "a works"},
		{"starting", agentRec{State: fleetv1.AgentState_AGENT_STATE_STARTING}, "a works"},
		{"idle", agentRec{State: idle}, ""},
		{"needs input", agentRec{State: fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT}, ""},
		{"running", agentRec{State: fleetv1.AgentState_AGENT_STATE_RUNNING}, ""},
		{"exited", agentRec{State: fleetv1.AgentState_AGENT_STATE_EXITED}, ""},
		{"resumed soon", agentRec{State: idle, limit: resumeAt(2 * time.Hour)}, "a is resumed at"},
		{"resumed in days", agentRec{State: idle, limit: resumeAt(72 * time.Hour)}, ""},
		{"limit, no auto-resume", agentRec{State: idle, limit: &fleetv1.UsageLimit{}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.a
			a.ID, a.Name, a.Adapter = "a1", "a", "test"
			m := awakeManager(t, &config.Config{}, &a)
			got := m.awakeReason(now)
			if tc.want == "" && got != "" || !strings.HasPrefix(got, tc.want) {
				t.Fatalf("awakeReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAwakeReasonBackgroundWorkflow(t *testing.T) {
	a := &agentRec{ID: "a1", Name: "a", Adapter: "test", State: fleetv1.AgentState_AGENT_STATE_IDLE, SessionID: "s1"}
	m := awakeManager(t, &config.Config{})
	m.agents[a.ID] = a
	dir := testAdapter{}.TranscriptDir("")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.Transcript = filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(a.Transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeRuns := func(runs ...workflow.Run) {
		b, _ := json.Marshal(runs)
		if err := os.WriteFile(filepath.Join(dir, "s1.runs"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeRuns(workflow.Run{ID: "wf_1", Status: workflow.Running, StartedMs: now.Add(-time.Minute).UnixMilli()})
	if got := m.awakeReason(now); got != "a runs workflow wf_1" {
		t.Fatalf("running workflow: awakeReason = %q", got)
	}
	writeRuns(workflow.Run{ID: "wf_1", Status: workflow.Running, StartedMs: now.Add(-time.Hour).UnixMilli()})
	if got := m.awakeReason(now); got != "" {
		t.Fatalf("stale workflow: awakeReason = %q", got)
	}
	writeRuns(workflow.Run{ID: "wf_1", Status: "completed", StartedMs: now.UnixMilli()})
	if got := m.awakeReason(now); got != "" {
		t.Fatalf("finished workflow: awakeReason = %q", got)
	}
}

func TestAwakeLoop(t *testing.T) {
	interval, command := awakeInterval, awakeCommand
	t.Cleanup(func() { awakeInterval, awakeCommand = interval, command })
	awakeInterval = 20 * time.Millisecond
	// Each keep-awake command writes its pid to pids.
	pids := filepath.Join(t.TempDir(), "pids")
	awakeCommand = func() *exec.Cmd {
		return exec.Command("sh", "-c", `echo $$ >> "$0"; exec sleep 60`, pids)
	}
	// running counts keep-awake commands that were started and that still run.
	running := func() (started, live int) {
		b, _ := os.ReadFile(pids)
		for _, f := range strings.Fields(string(b)) {
			started++
			if pid, _ := strconv.Atoi(f); pid > 0 && syscall.Kill(pid, 0) == nil {
				live++
			}
		}
		return started, live
	}

	a := &agentRec{ID: "a1", Adapter: "test", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	off := false
	cfg := &config.Config{}
	m := awakeManager(t, cfg, a)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.awakeLoop(ctx)
		close(done)
	}()
	setState := func(s fleetv1.AgentState) {
		m.mu.Lock()
		a.State = s
		m.mu.Unlock()
	}

	time.Sleep(5 * awakeInterval)
	if s, _ := running(); s != 0 {
		t.Fatalf("idle agent started %d keep-awake commands", s)
	}
	setState(fleetv1.AgentState_AGENT_STATE_WORKING)
	waitFor(t, "keep-awake command", func() bool { s, l := running(); return s == 1 && l == 1 })
	setState(fleetv1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "keep-awake command to end", func() bool { s, l := running(); return s == 1 && l == 0 })

	m.d.cfgMu.Lock()
	cfg.StayAwake = &off
	m.d.cfgMu.Unlock()
	setState(fleetv1.AgentState_AGENT_STATE_WORKING)
	time.Sleep(5 * awakeInterval)
	if s, _ := running(); s != 1 {
		t.Fatalf("stay_awake = false started %d keep-awake commands, want none", s-1)
	}

	m.d.cfgMu.Lock()
	cfg.StayAwake = nil
	m.d.cfgMu.Unlock()
	waitFor(t, "keep-awake command", func() bool { s, l := running(); return s == 2 && l == 1 })
	cancel()
	<-done
	if _, l := running(); l != 0 {
		t.Fatal("keep-awake command outlives the loop")
	}
}
