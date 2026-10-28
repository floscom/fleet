// Package gemini is the adapter for Google's Gemini CLI (`gemini`).
//
// The agent runs Gemini's interactive TUI, with the initial prompt given as
// --prompt-interactive. Activity state comes from Gemini's hooks (verified with 0.66.0). Gemini
// has no per-launch way to add hooks: a system settings file must be owned
// by root, and hooks from settings files do not run at all in folders the
// user has not trusted. Hooks of an extension run everywhere, so fleet
// installs one, ~/.gemini/extensions/fleet (extension.go). Its hooks call
// `fleet hook` only in sessions fleet started, which it tells by the
// environment Launch sets; in every other Gemini session they do nothing.
//
// In a new folder Gemini first asks whether to trust it, before any hook
// runs. In roots the user marked as trusted, Launch passes --skip-trust,
// which trusts the folder for that launch only; elsewhere DetectPrompt
// reports the dialog.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fleet/internal/adapter"
	"fleet/internal/adapter/detect"
)

// ID is the adapter id.
const ID = "gemini"

// The environment Launch gives the agent, which the extension's hooks read:
// the agent id, and the fleet binary to report to.
const (
	envAgent = "FLEET_GEMINI_AGENT"
	envHook  = "FLEET_GEMINI_HOOK"
)

type gemini struct {
	bin       *detect.Binary
	extraArgs []string
}

// New returns the Gemini adapter. binaryOverride replaces the binary search
// ("" searches PATH and well-known dirs); extraArgs are passed on every
// launch, before the request's own extra args.
func New(binaryOverride string, extraArgs []string) adapter.Adapter {
	return &gemini{
		bin:       &detect.Binary{Name: "gemini", Override: binaryOverride},
		extraArgs: extraArgs,
	}
}

func (g *gemini) ID() string          { return ID }
func (g *gemini) DisplayName() string { return "Gemini CLI" }

// Capabilities: resume would need a session id in LaunchRequest.
func (g *gemini) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{ActivityState: true, InitialPrompt: true, InitialImages: true}
}

func (g *gemini) Detect(ctx context.Context) adapter.Detection { return g.bin.Detect(ctx) }

// Launch installs fleet's extension and returns
//
//	gemini [--skip-trust] [extra...] [--model M] [--prompt-interactive=<prompt>]
//
// The prompt goes in one word with the flag: a prompt after "--" is never
// submitted, and one starting with "-" after a separate -i is read as a
// flag. No --session-id: Gemini restarts itself with the same arguments
// once the folder is trusted or it signed in, and the restarted one refuses
// the id the first one created. The session id comes from the hooks.
func (g *gemini) Launch(ctx context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	if req.FleetBinary == "" {
		return nil, errors.New("gemini: FleetBinary is required")
	}
	path := g.bin.Name
	if !req.Sandbox {
		var err error
		if path, err = g.bin.Find(); err != nil {
			return nil, fmt.Errorf("gemini: %w", err)
		}
	}
	if err := installExtension(geminiDir(req.Home)); err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	argv := []string{path}
	if req.TrustDir != "" {
		argv = append(argv, "--skip-trust")
	}
	argv = append(argv, g.extraArgs...)
	argv = append(argv, req.ExtraArgs...)
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	prompt := req.Prompt
	for _, img := range req.Images {
		// Gemini reads a file @-mentioned in the prompt into it.
		prompt += " @" + img
	}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		argv = append(argv, "--prompt-interactive="+prompt)
	}
	return &adapter.LaunchSpec{
		Argv: argv,
		Env:  map[string]string{envAgent: req.AgentID, envHook: req.FleetBinary},
	}, nil
}

// geminiDir is Gemini's own directory for the home directory home ("" is
// the daemon user's, or $GEMINI_CLI_HOME, which replaces it for Gemini).
func geminiDir(home string) string {
	if home == "" {
		home = os.Getenv("GEMINI_CLI_HOME")
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	return filepath.Join(home, ".gemini")
}
