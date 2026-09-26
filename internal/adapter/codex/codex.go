// Package codex is the adapter for the OpenAI Codex CLI (`codex`).
//
// The agent runs Codex's interactive TUI. Activity state comes from Codex
// lifecycle hooks (the "hooks" feature, stable in 0.154), installed per
// launch with `-c hooks.<Event>=[...]` overrides so ~/.codex is never
// modified. Hooks give turn start, permission requests and turn end; the
// older `notify` program only reports turn completion and would replace the
// user's own notify setting, so it is not installed (HandleHook still
// understands its payload).
//
// Codex only runs hooks the user has trusted. Trust is recorded as
// hooks.state."<source>:<event>:<group>:<handler>".trusted_hash, a SHA-256 of
// the normalized hook definition. Launch computes those hashes for its own
// hooks and passes them as another -c override, so fleet's hooks run without
// a review prompt while the user's untrusted hooks stay untrusted. If a
// future Codex changes the hash scheme, the TUI shows its "Hooks need review"
// dialog instead and activity state is missing until the user trusts them.
//
// Codex also asks "Do you trust the contents of this directory?" (default
// "Yes, continue") in directories it has not seen; the user answers it in the
// agent's terminal. The startup update prompt is disabled because it would
// block the TUI and offers to run an installer.
package codex

import (
	"context"
	"errors"
	"fmt"

	"fleet/internal/adapter"
	"fleet/internal/adapter/detect"
)

// ID is the adapter id.
const ID = "codex"

type codex struct {
	bin       *detect.Binary
	extraArgs []string
}

// New returns the Codex adapter. binaryOverride replaces the binary search
// ("" searches PATH and well-known dirs); extraArgs are passed on every
// launch, before the request's own extra args.
func New(binaryOverride string, extraArgs []string) adapter.Adapter {
	return &codex{
		bin:       &detect.Binary{Name: "codex", Override: binaryOverride},
		extraArgs: extraArgs,
	}
}

func (c *codex) ID() string          { return ID }
func (c *codex) DisplayName() string { return "Codex" }

// Capabilities: resume would need a session id in LaunchRequest.
func (c *codex) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{ActivityState: true, InitialPrompt: true}
}

func (c *codex) Detect(ctx context.Context) adapter.Detection { return c.bin.Detect(ctx) }

// Launch returns
//
//	codex -c check_for_update_on_startup=false -c hooks.<Event>=... -c hooks.state=... [extra...] [-- prompt]
//
// Codex creates its session id lazily, so SessionID is left empty; it is
// learned from the SessionStart hook.
func (c *codex) Launch(ctx context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	if req.FleetBinary == "" {
		return nil, errors.New("codex: FleetBinary is required")
	}
	path, err := c.bin.Find()
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	argv := []string{path, "-c", "check_for_update_on_startup=false"}
	for _, o := range HookOverrides(req.FleetBinary, req.AgentID) {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, c.extraArgs...)
	argv = append(argv, req.ExtraArgs...)
	if req.Prompt != "" {
		// "--" keeps a prompt starting with "-" from being read as a flag.
		argv = append(argv, "--", req.Prompt)
	}
	return &adapter.LaunchSpec{Argv: argv}, nil
}
