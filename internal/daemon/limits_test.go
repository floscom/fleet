package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
)

// limitAdapter is testAdapter whose sessions hit usage limits: the
// transcript line "limit <unix seconds>" stops a turn by the 5h limit
// resetting then ("limit 0": unknown), any other line goes on.
type limitAdapter struct{ testAdapter }

func (limitAdapter) ID() string { return "limited" }

func (limitAdapter) LimitLine(line []byte, l *adapter.Limit) {
	s, ok := strings.CutPrefix(string(line), "limit ")
	if !ok {
		*l = adapter.Limit{}
		return
	}
	*l = adapter.Limit{Hit: true, Window: "5h"}
	if n, _ := strconv.ParseInt(s, 10, 64); n > 0 {
		l.ResetsAt = time.Unix(n, 0)
	}
}

func TestResumeAfterLimit(t *testing.T) {
	interval, margin := limitInterval, resumeMargin
	limitInterval, resumeMargin = 50*time.Millisecond, 0
	t.Cleanup(func() { limitInterval, resumeMargin = interval, margin })

	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Adapter: "limited", Root: "code", ExtraArgs: []string{"cat"}})
	id, path := a.GetId(), "/api/agents/"+a.GetId()
	file := filepath.Join(testAdapter{}.TranscriptDir(""), a.GetSessionId()+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(line string) {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(f, line)
		f.Close()
	}
	// typed waits for one more "continue" on the screen (cat echoes it,
	// then prints it).
	seen := 0
	typed := func(what string) {
		t.Helper()
		waitFor(t, what, func() bool {
			var screen struct{ Screen string }
			e.webCall("GET", path+"/screen", "", &screen)
			n := strings.Count(screen.Screen, "continue")
			if n > seen {
				seen = n
				return true
			}
			return false
		})
	}

	screenOf := func() string {
		var screen struct{ Screen string }
		e.webCall("GET", path+"/screen", "", &screen)
		return screen.Screen
	}
	write("fix the tests")
	resets := time.Now().Add(1500 * time.Millisecond).Truncate(time.Second).Add(time.Second)
	write(fmt.Sprintf("limit %d", resets.Unix()))
	stopped := c.waitAgent(id, "stopped by the limit", func(a *fleetv1.Agent) bool { return a.GetUsageLimit() != nil })
	l := stopped.GetUsageLimit()
	if l.GetWindow() != "5h" || l.GetResetsAtMs() != resets.UnixMilli() || l.GetResumeAtMs() != resets.UnixMilli() || !l.GetAutoResume() {
		t.Fatalf("limit: %+v", l)
	}
	if stopped.GetState() != stateIdle || stopped.GetStateDetail() != "Usage limit reached (5h)" {
		t.Fatalf("state: %v %q", stopped.GetState(), stopped.GetStateDetail())
	}
	typed("the resume typed after the reset")
	if time.Now().Before(resets) {
		t.Fatal("resumed before the reset")
	}
	write("continue")
	c.waitAgent(id, "going on", func(a *fleetv1.Agent) bool { return a.GetUsageLimit() == nil })

	// Auto-resume off: shown, never typed, until resumed by hand.
	write("limit 0")
	c.waitAgent(id, "stopped again", func(a *fleetv1.Agent) bool { return a.GetUsageLimit() != nil })
	var r struct {
		Agent struct {
			UsageLimit *struct {
				ResumeAtMs int64
				AutoResume bool
			}
		}
	}
	if code := e.webCall("POST", path+"/resume", `{"auto":false}`, &r); code != 200 || r.Agent.UsageLimit == nil ||
		r.Agent.UsageLimit.AutoResume || r.Agent.UsageLimit.ResumeAtMs != 0 {
		t.Fatalf("auto off: %d %+v", code, r.Agent.UsageLimit)
	}
	seen = strings.Count(screenOf(), "continue")
	if code := e.webCall("POST", path+"/resume", `{"now":true}`, &r); code != 200 {
		t.Fatalf("resume now: %d", code)
	}
	typed("the resume typed by hand")
	write("continue")
	c.waitAgent(id, "going on again", func(a *fleetv1.Agent) bool { return a.GetUsageLimit() == nil })
	var fail struct{ Error string }
	if code := e.webCall("POST", path+"/resume", `{"now":true}`, &fail); code != 400 || !strings.Contains(fail.Error, "not stopped") {
		t.Fatalf("resume a working agent: %d %+v", code, fail)
	}
}

func TestExhaustedWindow(t *testing.T) {
	t1, t2 := time.Unix(1000, 0), time.Unix(2000, 0)
	u := &adapter.Usage{Limits: []adapter.UsageLimit{
		{Label: "5h", Percent: 100, ResetsAt: t1},
		{Label: "week", Percent: 100, ResetsAt: t2},
		{Label: "week Opus", Percent: 30, ResetsAt: t2},
	}}
	if w, r, ok := exhaustedWindow(u, ""); w != "week" || !r.Equal(t2) || !ok {
		t.Errorf("latest: %q %v %v", w, r, ok)
	}
	if w, r, _ := exhaustedWindow(u, "5h"); w != "5h" || !r.Equal(t1) {
		t.Errorf("named: %q %v", w, r)
	}
	if w, _, ok := exhaustedWindow(&adapter.Usage{Limits: u.Limits[2:]}, "5h"); w != "" || !ok {
		t.Errorf("none used up: %q %v", w, ok)
	}
	if _, _, ok := exhaustedWindow(&adapter.Usage{}, ""); ok {
		t.Error("no windows known")
	}
}
