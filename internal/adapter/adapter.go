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
//   - workflows (subagents a session runs in the background): implement
//     Workflower.
//   - a choice of model and reasoning effort: implement Modeler, and
//     ModelSwitcher to switch a running session.
//   - questions answered in a form on the dashboard: implement Asker.
//   - images in the chat: implement TranscriptImager.
//   - how much of the account's plan limits is used: implement Usager.
package adapter

import (
	"context"
	"sort"
	"sync"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/ask"
	"fleet/internal/transcript"
	"fleet/internal/workflow"
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

// Asker is implemented by adapters whose CLI asks the user questions with
// choices in a dialog and whose hook can answer them: the hook runs
// `fleet hook --wait` (hookcmd.WaitCommand), the dashboard shows the
// questions as a form, and the hook prints the answer. The dialog stays
// open in the terminal meanwhile: whichever answers first wins.
type Asker interface {
	// Questions returns the questions a hook event asks, ok=false if it
	// asks none the dashboard can answer.
	Questions(ev HookEvent) (qs []ask.Question, ok bool)
	// AnswerOutput is what the hook of ev prints to answer its questions
	// (a was checked against them).
	AnswerOutput(ev HookEvent, a ask.Answer) ([]byte, error)
}

// TranscriptImager is implemented by Transcripters whose transcripts hold
// images inline (transcript.Image says where).
type TranscriptImager interface {
	// TranscriptImage returns image n of a transcript line: its media type
	// and data.
	TranscriptImage(line []byte, n int) (mediaType string, data []byte, ok bool)
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

// Workflower is implemented by Transcripters whose CLI runs workflows:
// scripts that start many subagents, in phases, in the background of a
// session. The dashboard shows their progress and each subagent's
// transcript.
type Workflower interface {
	// Workflows lists the workflow runs of the session whose transcript is
	// at path, oldest first. It is called again and again while a run goes
	// on, so it should read only what changed.
	Workflows(path string) []workflow.Run
	// WorkflowTranscript returns the transcript of agent agent of run run
	// of that session, and the parser for its lines.
	WorkflowTranscript(path, run, agent string) (string, transcript.Parser, bool)
}

// Modeler is implemented by adapters whose CLI runs a chosen model at a
// chosen reasoning effort: Launch honours LaunchRequest.Model and .Effort,
// and the dashboard shows which ones a session runs at.
type Modeler interface {
	// Models lists the models and effort levels to offer.
	Models() Models
	// ModelLine updates m from one line of a session's transcript (see
	// Transcripter) that names the model or effort in use, such as a reply
	// or a switch to another model. It is given every line, in order; m
	// starts as what the agent was launched with.
	ModelLine(line []byte, m *SessionModel)
}

// ModelSwitcher is implemented by Modelers that can switch a running
// session to another model or effort, for that session only.
type ModelSwitcher interface {
	// SwitchModel switches the session running in term. model and effort
	// are IDs from Models; "" keeps the current one.
	SwitchModel(ctx context.Context, term Terminal, model, effort string) error
}

// Usager is implemented by adapters whose CLI logs into a subscription
// with usage limits (a 5-hour session, a week, ...): the dashboard shows
// how much of each the daemon user's account has used.
type Usager interface {
	// Usage asks the provider for the current usage of the account the CLI
	// is logged in with. Errors are shown to the user as the reason the
	// usage is unknown.
	Usage(ctx context.Context) (*Usage, error)
}

// Usage is how much of an account's plan limits is used.
type Usage struct {
	// Plan names the subscription, e.g. "max 20x"; may be empty.
	Plan   string
	Limits []UsageLimit
}

// UsageLimit is one usage window of a plan.
type UsageLimit struct {
	// Label names the window, e.g. "5h", "week" or "week Fable".
	Label string
	// Percent is how much of it is used, 0-100.
	Percent float64
	// ResetsAt is when the window starts over; zero if unknown.
	ResetsAt time.Time
}

// Terminal is the terminal of a live agent.
type Terminal interface {
	// Screen is its visible screen, as text.
	Screen(ctx context.Context) (string, error)
	// Type types text, then presses Enter if submit.
	Type(ctx context.Context, text string, submit bool) error
	// Press presses keys, by tmux key name ("Enter", "Down", "s").
	Press(ctx context.Context, keys ...string) error
}

// Models is what a Modeler offers.
type Models struct {
	Models []Model
	// Efforts are the reasoning effort levels, least first.
	Efforts []Choice
}

// Model is a model a Modeler offers.
type Model struct {
	ID, Label string
	// Efforts are the IDs of the efforts it runs at, none if it has no
	// effort levels.
	Efforts []string
}

// Choice is one of a list of options.
type Choice struct{ ID, Label string }

// SessionModel is the model and effort a session runs at.
type SessionModel struct {
	// Model is the ID of one of the Modeler's models, "" if unknown or none
	// matches; Name is what the CLI calls it, e.g. "claude-opus-5-5".
	Model, Name string
	// Effort is the ID of one of its efforts, "" if unknown or none.
	Effort string
}

// Find returns the model with ID id.
func (ms Models) Find(id string) (Model, bool) {
	for _, m := range ms.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// HasEffort reports whether id is one of the effort levels.
func (ms Models) HasEffort(id string) bool {
	for _, e := range ms.Efforts {
		if e.ID == id {
			return true
		}
	}
	return false
}

// Capabilities mirrors fleetv1.AdapterCapabilities.
type Capabilities struct {
	ActivityState bool
	InitialPrompt bool
	Resume        bool
	// InitialImages: LaunchRequest.Images are attached to the prompt.
	InitialImages bool
}

// Proto converts to the wire type.
func (c Capabilities) Proto() *fleetv1.AdapterCapabilities {
	return &fleetv1.AdapterCapabilities{
		ActivityState: c.ActivityState,
		InitialPrompt: c.InitialPrompt,
		Resume:        c.Resume,
		InitialImages: c.InitialImages,
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
	// Images are paths of image files to attach to Prompt, for adapters
	// with InitialImages.
	Images []string
	// Model and Effort are the model and reasoning effort to run at, for
	// Modelers: IDs from Models (Model may also be any name the CLI takes).
	// "" leaves it to the CLI's own default.
	Model, Effort string
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
