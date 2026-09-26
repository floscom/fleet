// Package claude is the adapter for Claude Code (the `claude` CLI).
//
// The agent runs Claude's own interactive TUI (not -p). Activity state comes
// from Claude Code hooks: Launch writes a settings file into the agent's
// state dir and passes it with --settings. Settings given that way are merged
// with the user's own settings; hook lists from all sources are combined, so
// the user's hooks keep running (verified with Claude Code 2.1.280).
//
// Claude Code shows a "Do you trust this folder?" dialog, defaulting to
// "No, exit", the first time it starts in a directory that neither it nor an
// ancestor was trusted in. No hook runs before it is answered; the user
// answers it in the agent's terminal. There is no flag to skip it.
package claude

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"fleet/internal/adapter"
	"fleet/internal/adapter/detect"
)

// ID is the adapter id.
const ID = "claude"

// SettingsFile is the name of the generated settings file in the state dir.
const SettingsFile = "claude-settings.json"

// unsetEnv is removed from the agent's environment.
//
//   - ANTHROPIC_API_KEY: when set, Claude Code bills that API key per token
//     instead of using the logged-in subscription, without asking. A key
//     exported in the daemon's environment for some other tool would silently
//     turn every agent into metered usage.
//   - CLAUDECODE: set inside Claude Code sessions; if the daemon was started
//     from one, agents would believe they are nested sessions.
var unsetEnv = []string{"ANTHROPIC_API_KEY", "CLAUDECODE"}

type claude struct {
	bin       *detect.Binary
	extraArgs []string
}

// New returns the Claude Code adapter. binaryOverride replaces the binary
// search ("" searches PATH and well-known dirs); extraArgs are passed on
// every launch, before the request's own extra args.
func New(binaryOverride string, extraArgs []string) adapter.Adapter {
	return &claude{
		bin:       &detect.Binary{Name: "claude", Override: binaryOverride},
		extraArgs: extraArgs,
	}
}

func (c *claude) ID() string          { return ID }
func (c *claude) DisplayName() string { return "Claude Code" }

// Capabilities: resume would need a session id in LaunchRequest.
func (c *claude) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{ActivityState: true, InitialPrompt: true}
}

func (c *claude) Detect(ctx context.Context) adapter.Detection { return c.bin.Detect(ctx) }

// Launch writes the hook settings file and returns
//
//	claude --session-id <uuid> --settings <file> [extra...] [-- prompt]
//
// When the extra args resume a conversation (-c/--continue, -r/--resume),
// --session-id is left out (claude rejects it there without --fork-session)
// and the session id is learned from the SessionStart hook instead.
func (c *claude) Launch(ctx context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	if req.FleetBinary == "" || req.StateDir == "" {
		return nil, errors.New("claude: FleetBinary and StateDir are required")
	}
	path, err := c.bin.Find()
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	settings := filepath.Join(req.StateDir, SettingsFile)
	if err := writeSettings(settings, req.FleetBinary, req.AgentID); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	extra := append(slices.Clone(c.extraArgs), req.ExtraArgs...)
	argv := []string{path}
	var sid string
	if !resumes(extra) {
		if sid, err = newUUID(); err != nil {
			return nil, fmt.Errorf("claude: session id: %w", err)
		}
		argv = append(argv, "--session-id", sid)
	}
	argv = append(argv, "--settings", settings)
	argv = append(argv, extra...)
	if req.Prompt != "" {
		// "--" keeps a prompt starting with "-" from being read as a flag.
		argv = append(argv, "--", req.Prompt)
	}
	return &adapter.LaunchSpec{
		Argv:      argv,
		UnsetEnv:  append([]string(nil), unsetEnv...),
		SessionID: sid,
	}, nil
}

// resumes reports whether args ask claude to continue or resume a session.
func resumes(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		switch {
		case a == "-c", a == "--continue", a == "-r", a == "--resume",
			strings.HasPrefix(a, "--resume="), strings.HasPrefix(a, "--continue="):
			return true
		case len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:2], "cr"):
			// A short-flag group such as -rc or -r<id>.
			return true
		}
	}
	return false
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
