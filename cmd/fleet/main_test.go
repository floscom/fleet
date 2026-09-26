package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/wire"
)

func TestParseRunPath(t *testing.T) {
	cwd, _ := os.Getwd()
	tests := []struct {
		in, host            string
		root, rel, abs, err string
	}{
		{in: "code:api/x", root: "code", rel: "api/x"},
		{in: "code:", root: "code", rel: "."},
		{in: "/srv/app", abs: "/srv/app"},
		{in: "/srv/a:b", abs: "/srv/a:b"},
		{in: ".", abs: cwd},
		{in: "sub/dir", abs: filepath.Join(cwd, "sub/dir")},
		{in: "./a:b", abs: filepath.Join(cwd, "a:b")},
		{in: "sub", host: "box", err: "relative"},
		{in: "/srv/app", host: "box", abs: "/srv/app"},
		{in: "code:api", host: "box", root: "code", rel: "api"},
	}
	for _, tt := range tests {
		host = tt.host
		root, rel, abs, err := parseRunPath(tt.in)
		host = ""
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q: err = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || root != tt.root || rel != tt.rel || abs != tt.abs {
			t.Errorf("%q = (%q, %q, %q, %v), want (%q, %q, %q)", tt.in, root, rel, abs, err, tt.root, tt.rel, tt.abs)
		}
	}
}

func TestParseHookArgs(t *testing.T) {
	tests := []struct {
		args                  []string
		agent, adapter, event string
		payload               string
		hasPayload            bool
	}{
		{args: []string{"--agent", "a1", "--adapter", "claude", "Stop"}, agent: "a1", adapter: "claude", event: "Stop"},
		{args: []string{"--agent=a1", "-adapter=codex", "notify", `{"type":"x"}`}, agent: "a1", adapter: "codex", event: "notify", payload: `{"type":"x"}`, hasPayload: true},
		{args: []string{"Stop", "--agent", "a1", "--bogus"}, agent: "a1", event: "Stop"},
		{args: []string{"--agent", "a1", "--", "-weird", "{}"}, agent: "a1", event: "-weird", payload: "{}", hasPayload: true},
		{args: nil},
	}
	for _, tt := range tests {
		h := parseHookArgs(tt.args)
		if h.agent != tt.agent || h.adapter != tt.adapter || h.event != tt.event || string(h.payload) != tt.payload || h.hasPayload != tt.hasPayload {
			t.Errorf("parseHookArgs(%q) = %+v", tt.args, h)
		}
	}
}

func TestCompactDuration(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:     "0s",
		42 * time.Second: "42s",
		5 * time.Minute:  "5m",
		3 * time.Hour:    "3h",
		47 * time.Hour:   "47h",
		72 * time.Hour:   "3d",
	}
	for d, want := range tests {
		if got := compactDuration(d); got != want {
			t.Errorf("compactDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestRunHook checks that `fleet hook` delivers the event with the stdin
// payload, and stays silent and harmless when no daemon runs.
func TestRunHook(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())

	// No daemon: must return promptly without panicking.
	start := time.Now()
	runHook([]string{"--agent", "a1", "--adapter", "claude", "Stop", "{}"}, nil)
	if time.Since(start) > 3*time.Second {
		t.Fatal("runHook took too long without a daemon")
	}

	if _, err := config.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", config.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan *fleetv1.HookEvent, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		conn := wire.NewConn(nc)
		defer conn.Close()
		_ = conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Hello{Hello: &fleetv1.ServerHello{ProtocolVersion: fleetv1.ProtocolVersion_PROTOCOL_VERSION_1}}})
		m, err := conn.RecvClient()
		if err != nil {
			return
		}
		got <- m.GetHook()
		_ = conn.SendServer(&fleetv1.ServerMessage{Id: m.Id, Msg: &fleetv1.ServerMessage_Hook{Hook: &fleetv1.HookResponse{}}})
	}()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(`{"hook_event_name":"Stop"}`)
	w.Close()
	runHook([]string{"--agent", "a1", "--adapter", "claude", "Stop"}, r)
	r.Close()
	select {
	case ev := <-got:
		if ev.GetAgentId() != "a1" || ev.GetAdapter() != "claude" || ev.GetEvent() != "Stop" || string(ev.GetPayload()) != `{"hook_event_name":"Stop"}` {
			t.Fatalf("hook event %v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no hook event")
	}
}

func TestReadWithTimeout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	w.WriteString("partial")
	start := time.Now()
	b := readWithTimeout(r, 1<<20, 100*time.Millisecond) // writer never closes
	if string(b) != "partial" || time.Since(start) > time.Second {
		t.Fatalf("got %q after %v", b, time.Since(start))
	}
	if b := readWithTimeout(strings.NewReader(strings.Repeat("x", 100)), 10, time.Second); len(b) != 10 {
		t.Fatalf("cap: got %d bytes", len(b))
	}
}

// Configured adapter args are added by the daemon on every launch, so the
// adapters themselves must not add them again (codex rejects repeated flags).
func TestRegistryDoesNotDuplicateConfigArgs(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Adapters: map[string]config.AdapterConfig{
		"claude": {Binary: bin, Args: []string{"--model", "cfg-model"}},
		"codex":  {Binary: bin, Args: []string{"-m", "cfg-model"}},
	}}
	reg := registry(cfg)
	for _, id := range []string{"claude", "codex"} {
		ad, ok := reg.Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		// The daemon passes config args + request args as ExtraArgs.
		extra := append(slices.Clone(cfg.Adapters[id].Args), "--req")
		spec, err := ad.Launch(context.Background(), adapter.LaunchRequest{
			AgentID: "a", ExtraArgs: extra, FleetBinary: "/f", StateDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range spec.Argv {
			if a == "cfg-model" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: configured args appear %d times in %q", id, n, spec.Argv)
		}
	}
}
