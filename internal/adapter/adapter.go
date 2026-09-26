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
package adapter

import (
	"context"
	"sort"
	"sync"

	fleetv1 "fleet/gen/fleetv1"
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
