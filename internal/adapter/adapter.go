// Package adapter defines how fleet launches and observes a specific agent
// CLI (claude, codex, ...). To add an agent, implement Adapter in a new
// sub-package and register it in cmd/fleet (see Registry).
//
// Fleet is terminal-first: an adapter only has to produce the command line
// that runs the agent's own interactive TUI inside a tmux session. Everything
// else is optional:
//
//   - activity state (working / idle / needs input): install hooks that call
//     `fleet hook --agent <id> --adapter <adapter> <event>` and translate those
//     events in HandleHook.
//   - initial prompt, resume: advertise them in Capabilities.
//   - startup dialogs no hook reports (folder trust): implement
//     PromptDetector, and honour LaunchRequest.TrustDir to skip them.
//   - sandboxes: honour LaunchRequest.Sandbox and LaunchRequest.Home.
//   - chat view: implement Transcripter, and report the transcript file in
//     StateUpdate.Transcript.
//   - input from the dashboard into dialogs: implement DialogDetector.
package adapter

import (
	"context"
	"sort"
	"sync"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

// Adapter launches one kind of agent CLI.
type Adapter interface {
	// ID is the stable identifier used in the protocol, e.g. "claude".
	ID() string
	// DisplayName is shown in UIs, e.g. "Claude Code".
	DisplayName() string
	// Capabilities advertises optional features.
	Capabilities() Capabilities
	// Detect locates the CLI on this machine. It must be cheap enough to call
	// on every ListAdapters request (implementations may cache).
	Detect(ctx context.Context) Detection
	// Launch builds the command that runs the agent. It must not start any
	// process itself; the daemon runs Command inside tmux.
	Launch(ctx context.Context, req LaunchRequest) (*LaunchSpec, error)
	// HandleHook translates an event delivered by `fleet hook` into a state
	// change. Return ok=false to ignore the event.
	HandleHook(ev HookEvent) (update StateUpdate, ok bool)
}

// PromptDetector is implemented by adapters whose CLI can wait on a dialog
// that no hook reports, such as a folder trust prompt at startup. The daemon
// shows the visible screen of a starting agent to DetectPrompt and reports
// NEEDS_INPUT with the returned detail while it matches.
type PromptDetector interface {
	DetectPrompt(screen string) (detail string, ok bool)
}

// DialogDetector is implemented by adapters that can tell from the screen
// whether the CLI shows a dialog (a question, a permission, a menu), where
// Enter picks the highlighted option instead of sending what was typed.
// The daemon does not press Enter after text typed into a dialog from the
// dashboard, and notices dialogs that closed without a hook saying so.
type DialogDetector interface {
	DialogOpen(screen string) bool
}

// AuthProvider is implemented by adapters whose CLI keeps its login in a
// file. With [sandbox] auth, the daemon keeps that file in sync between the
// daemon user's home and the sandbox home, so sandboxed agents use the
// host's login.
type AuthProvider interface {
	// AuthFile is the credentials file for a home directory. home "" means
	// the daemon user's own, honouring the CLI's config dir variable.
	AuthFile(home string) string
	// LockAuth takes the lock the CLI holds while writing file, if it uses
	// one, so a sync never interleaves with the CLI's own write.
	LockAuth(ctx context.Context, file string) (unlock func(), err error)
}

// Transcripter is implemented by adapters whose CLI writes the conversation
// to a transcript file (JSONL), which the dashboard shows as a chat.
type Transcripter interface {
	// TranscriptDir is the directory the CLI keeps its transcripts in, for
	// the home directory home ("" means the daemon user's own, honouring the
	// CLI's config dir variable). The daemon only reads transcripts inside it.
	TranscriptDir(home string) string
	// FindTranscript looks in dir (a TranscriptDir) for the transcript of
	// session sessionID, for agents whose hooks never reported its path.
	FindTranscript(dir, sessionID string) (path string, ok bool)
	// ParseTranscript turns one line of a transcript into chat entries.
	ParseTranscript(line []byte) []transcript.Entry
}

// Capabilities mirrors fleetv1.AdapterCapabilities.
type Capabilities struct {
	ActivityState bool
	InitialPrompt bool
	Resume        bool
}

// Proto converts to the wire type.
func (c Capabilities) Proto() *fleetv1.AdapterCapabilities {
	return &fleetv1.AdapterCapabilities{
		ActivityState: c.ActivityState,
		InitialPrompt: c.InitialPrompt,
		Resume:        c.Resume,
	}
}

// Detection is the result of Adapter.Detect.
type Detection struct {
	Available bool
	Path      string
	Version   string
	// Reason explains why Available is false.
	Reason string
}

// LaunchRequest is everything an adapter needs to build its command.
type LaunchRequest struct {
	// AgentID is the fleet agent id; pass it to `fleet hook --agent`.
	AgentID string
	// AgentName is the human-friendly agent name.
	AgentName string
	// Cwd is the directory the agent runs in (already validated).
	Cwd string
	// Prompt is the optional initial prompt.
	Prompt string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// FleetBinary is the absolute path of the running fleet executable, for
	// hook commands.
	FleetBinary string
	// StateDir is a private per-agent directory the adapter may write to
	// (e.g. generated settings files). Removed when the agent is forgotten.
	StateDir string
	// TrustDir, if set, is a directory the user has chosen to trust: the
	// agent's git repository root (the main one for worktrees) or, outside
	// git, its directory. The adapter should keep the CLI from asking
	// whether to trust it.
	TrustDir string
	// Sandbox is set when the command runs in a container whose image
	// provides the CLI on its PATH: Argv[0] should be the bare command name,
	// and the CLI need not be installed on the host. Every path in this
	// request is the same inside the container.
	Sandbox bool
	// Home, if set, is the host directory the agent uses as its home
	// directory (HOME inside a sandbox). Per-user config the adapter writes,
	// such as folder trust, goes there instead of the daemon user's home.
	Home string
}

// LaunchSpec is the command the daemon runs in tmux.
type LaunchSpec struct {
	// Argv[0] should be an absolute path.
	Argv []string
	// Env is added to the agent's environment.
	Env map[string]string
	// UnsetEnv lists variables to remove from the inherited environment
	// (e.g. ANTHROPIC_API_KEY to avoid silent metered billing).
	UnsetEnv []string
	// SessionID is the adapter's own session id if known up front.
	SessionID string
}

// HookEvent is what `fleet hook` delivered.
type HookEvent struct {
	AgentID string
	Event   string
	Payload []byte
}

// StateUpdate is a state change derived from a hook.
type StateUpdate struct {
	State fleetv1.AgentState
	// Detail is a short human-readable note (e.g. the permission being asked).
	Detail string
	// SessionID, if non-empty, records the adapter's session id.
	SessionID string
	// Transcript, if non-empty, is the session's transcript file as the CLI
	// sees it (inside a sandbox: a path in the container). See Transcripter.
	Transcript string
	// Subagent names the subagent the event came from, "" for the agent
	// itself, and Call the tool call it is about ("" if none, such as a
	// turn ending), summarized the same way by every event of that call.
	// Tool calls go on while a dialog waits for the user (in parallel, or
	// in background subagents), so a NEEDS_INPUT update opens a dialog for
	// its Subagent and Call, which only their own updates close.
	Subagent string
	Call     string
}

// Registry holds the adapters known to the daemon.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns a registry containing as.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range as {
		r.Register(a)
	}
	return r
}

// Register adds or replaces an adapter.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.ID()] = a
}

// Get returns the adapter with the given id.
func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[id]
	return a, ok
}

// All returns adapters sorted by id.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}
