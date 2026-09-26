package shell

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"fleet/internal/adapter"
)

func fakeShell(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fsh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestShellSelection(t *testing.T) {
	envShell, override := fakeShell(t), fakeShell(t)
	tests := []struct {
		name, override, env, want string
	}{
		{"override wins", override, envShell, override},
		{"$SHELL", "", envShell, envShell},
		{"default", "", "", DefaultShell},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHELL", tt.env)
			a := New(tt.override)
			d := a.Detect(context.Background())
			if !d.Available || d.Path != tt.want {
				t.Fatalf("Detect = %+v", d)
			}
			spec, err := a.Launch(context.Background(), adapter.LaunchRequest{
				AgentID: "a", Cwd: "/w", Prompt: "ignored", ExtraArgs: []string{"-i"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{tt.want, "-l", "-i"}; !slices.Equal(spec.Argv, want) {
				t.Errorf("Argv = %q, want %q", spec.Argv, want)
			}
		})
	}
}

func TestMissingShell(t *testing.T) {
	a := New(filepath.Join(t.TempDir(), "nosh"))
	if d := a.Detect(context.Background()); d.Available || d.Reason == "" {
		t.Errorf("Detect = %+v", d)
	}
	if _, err := a.Launch(context.Background(), adapter.LaunchRequest{}); err == nil {
		t.Error("Launch: no error")
	}
}

func TestNoHooks(t *testing.T) {
	a := New("")
	if a.ID() != "shell" || a.Capabilities() != (adapter.Capabilities{}) {
		t.Errorf("%s %+v", a.ID(), a.Capabilities())
	}
	if _, ok := a.HandleHook(adapter.HookEvent{Event: "Stop"}); ok {
		t.Error("HandleHook accepted an event")
	}
}
