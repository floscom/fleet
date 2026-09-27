// Package shell is the simplest adapter: an interactive login shell in the
// agent's directory. It installs no hooks and advertises no capabilities, so
// the daemon only knows whether it is running. Useful for testing and as a
// template for new adapters.
package shell

import (
	"context"
	"fmt"
	"os"

	"fleet/internal/adapter"
	"fleet/internal/adapter/detect"
)

// ID is the adapter id.
const ID = "shell"

// DefaultShell is used when neither an override nor $SHELL is set.
const DefaultShell = "/bin/sh"

type shell struct {
	override string
}

// New returns the shell adapter. shellOverride replaces $SHELL.
func New(shellOverride string) adapter.Adapter {
	return &shell{override: shellOverride}
}

func (s *shell) ID() string                         { return ID }
func (s *shell) DisplayName() string                { return "Shell" }
func (s *shell) Capabilities() adapter.Capabilities { return adapter.Capabilities{} }

// binary resolves the shell each time so a changed $SHELL is picked up.
func (s *shell) binary() *detect.Binary {
	name := s.override
	if name == "" {
		name = os.Getenv("SHELL")
	}
	if name == "" {
		name = DefaultShell
	}
	return &detect.Binary{Name: name, Override: name, NoVersion: true}
}

func (s *shell) Detect(ctx context.Context) adapter.Detection { return s.binary().Detect(ctx) }

// sandboxScript starts bash as a login shell where the image has it, else sh.
const sandboxScript = `if command -v bash >/dev/null 2>&1; then exec bash -l "$@"; fi; exec sh -l "$@"`

// Launch returns `<shell> -l`; the daemon starts it in req.Cwd. Prompt and
// hooks are not supported and ignored. In a sandbox the host's $SHELL means
// nothing, so bash (or sh) from the image is used.
func (s *shell) Launch(ctx context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	if req.Sandbox {
		argv := append([]string{"/bin/sh", "-c", sandboxScript, "sh"}, req.ExtraArgs...)
		return &adapter.LaunchSpec{Argv: argv}, nil
	}
	path, err := s.binary().Find()
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	argv := append([]string{path, "-l"}, req.ExtraArgs...)
	return &adapter.LaunchSpec{Argv: argv}, nil
}

// HandleHook ignores everything: the shell has no hooks.
func (s *shell) HandleHook(adapter.HookEvent) (adapter.StateUpdate, bool) {
	return adapter.StateUpdate{}, false
}
