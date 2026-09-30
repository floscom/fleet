// Package claude is the adapter for Claude Code (the `claude` CLI).
//
// The agent runs Claude's own interactive TUI (not -p). Activity state comes
// from Claude Code hooks: Launch writes a settings file into the agent's
// state dir and passes it with --settings. Settings given that way are merged
// with the user's own settings; hook lists from all sources are combined, so
// the user's hooks keep running (verified with Claude Code 2.1.280).
//
// Claude Code shows a "Do you trust this folder?" dialog, defaulting to
// "No, exit", the first time it starts in an untrusted directory (see
// trust.go). No hook runs before it is answered, and there is no flag to skip
// it. In roots the user marked as trusted, Launch records the trust in
// Claude's global config first; elsewhere DetectPrompt reports the dialog so
// the user can answer it in the agent's terminal.
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
	path := c.bin.Name
	if !req.Sandbox {
		var err error
		if path, err = c.bin.Find(); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	if req.Home != "" {
		// A private home starts without Claude's config; skip onboarding
		// there so the agent starts straight into its session.
		if err := seedConfig(filepath.Join(req.Home, ".claude.json")); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	if req.TrustDir != "" {
		file, err := globalConfigFile()
		if req.Home != "" {
			file, err = filepath.Join(req.Home, ".claude.json"), nil
		}
		if err == nil {
			err = trustFolder(ctx, file, req.TrustDir)
		}
		if err != nil {
			return nil, fmt.Errorf("claude: trust %s: %w", req.TrustDir, err)
		}
	}
	settings := filepath.Join(req.StateDir, SettingsFile)
	if err := writeSettings(settings, req.FleetBinary, req.AgentID); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	extra := append(slices.Clone(c.extraArgs), req.ExtraArgs...)
	argv := []string{path}
	var sid string
	var err error
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

// DetectPrompt recognizes the folder trust dialog.
func (c *claude) DetectPrompt(screen string) (string, bool) {
	if strings.Contains(screen, "Yes, I trust this folder") {
		return "Claude asks whether to trust this folder; answer in its terminal", true
	}
	return "", false
}

// dialogHint is one of the " · "-separated hints on the last line of every
// Claude dialog: questions ("Enter to select · ↑/↓ to navigate · Esc to
// cancel"), permissions ("Esc to cancel · Tab to amend"), menus and the
// trust prompt. Status lines say "esc to cancel" and "esc to interrupt",
// lower case.
const dialogHint = "Esc to cancel"

// footerLines is how many non-blank lines at the bottom of the screen may
// hold a dialog's footer: below it come at most queued messages and the
// status line.
const footerLines = 8

// DialogOpen reports whether a dialog's footer is at the bottom of the
// screen: a line of hints, one of them dialogHint. Higher up, or amid
// other words, the text is part of the conversation.
func (c *claude) DialogOpen(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i, n := len(lines)-1, 0; i >= 0 && n < footerLines; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if slices.Contains(strings.Split(l, " · "), dialogHint) {
			return true
		}
		n++
	}
	return false
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
