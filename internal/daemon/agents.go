package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
)

const (
	// maxHistory is how many finished agents are kept until forgotten.
	maxHistory = 200
	// subscriberBuffer is the per-subscriber event backlog beyond the snapshot.
	subscriberBuffer = 256
)

// agentRec is one agent in the registry, persisted in agents.json.
type agentRec struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Adapter     string             `json:"adapter"`
	Path        string             `json:"path"`
	Root        string             `json:"root"`
	Cwd         string             `json:"cwd"`
	Isolation   fleetv1.Isolation  `json:"isolation"`
	Branch      string             `json:"branch,omitempty"`
	State       fleetv1.AgentState `json:"state"`
	StateDetail string             `json:"state_detail,omitempty"`
	HasExitCode bool               `json:"has_exit_code,omitempty"`
	ExitCode    int32              `json:"exit_code,omitempty"`
	CreatedAtMs int64              `json:"created_at_ms"`
	UpdatedAtMs int64              `json:"updated_at_ms"`
	// StartedAtMs is when the tmux session was created.
	StartedAtMs int64  `json:"started_at_ms,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	TmuxSession string `json:"tmux_session"`
	// Worktree is the git worktree the daemon created (WORKTREE isolation).
	Worktree        string `json:"worktree,omitempty"`
	WorktreeRemoved bool   `json:"worktree_removed,omitempty"`
	// ScreenPrompt is set while NEEDS_INPUT comes from a dialog seen on the
	// screen (adapter.PromptDetector) rather than from a hook.
	ScreenPrompt bool `json:"screen_prompt,omitempty"`
	// Dialogs are the dialogs hooks report open, oldest first (the CLI
	// shows them one at a time, in that order). While any is open the agent
	// is NEEDS_INPUT; then it returns to OwnState, the state its own hooks
	// last reported. See agentRec.applyHook.
	Dialogs   []dialog           `json:"dialogs,omitempty"`
	OwnState  fleetv1.AgentState `json:"own_state,omitempty"`
	OwnDetail string             `json:"own_detail,omitempty"`
	// Sandbox is SANDBOX_DOCKER for containerized agents; records written
	// before sandboxes existed have 0, meaning SANDBOX_NONE.
	Sandbox fleetv1.Sandbox `json:"sandbox,omitempty"`
	// Container is the Docker container name of a sandboxed agent.
	Container string `json:"container,omitempty"`
	// StateDir is the state dir of a sandboxed agent, inside the sandbox
	// dir; other agents use <home>/agents/<id> (see stateDir).
	StateDir string `json:"state_dir,omitempty"`
	// CloneURL is the repository cloned for CLONE isolation; the clone
	// itself is in Worktree.
	CloneURL string `json:"clone_url,omitempty"`
	// Transcript is the agent's transcript file on this host, as reported
	// by its hooks (see adapter.Transcripter).
	Transcript string `json:"transcript,omitempty"`
	// Model and Effort are what the agent was launched with or last
	// switched to from fleet, "" for the CLI's default (adapter.Modeler).
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// AutoResume switches resuming the agent after a usage limit for this
	// agent; nil follows [limits] auto_resume.
	AutoResume *bool `json:"auto_resume,omitempty"`

	// limit is the usage limit that stopped the agent (limits.go).
	limit *fleetv1.UsageLimit

	attached int32
	// dialogGone counts the screens in a row that showed none of Dialogs
	// (see watchPrompts).
	dialogGone int
	// busy is set while RunAgent or KillAgent is working on the agent
	// outside the lock; reconcile leaves busy agents alone.
	busy bool
	// readyGen is manager.readyGen when busy was last cleared; reconcile
	// skips agents that became ready after its tmux snapshot was taken.
	readyGen uint64
}

func (a *agentRec) live() bool { return isLive(a.State) }

func isLive(s fleetv1.AgentState) bool {
	return s >= fleetv1.AgentState_AGENT_STATE_STARTING && s <= fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
}

func (a *agentRec) proto() *fleetv1.Agent {
	return &fleetv1.Agent{
		Id:              a.ID,
		Name:            a.Name,
		Adapter:         a.Adapter,
		Path:            a.Path,
		Root:            a.Root,
		Cwd:             a.Cwd,
		Isolation:       a.Isolation,
		Branch:          a.Branch,
		State:           a.State,
		StateDetail:     a.StateDetail,
		HasExitCode:     a.HasExitCode,
		ExitCode:        a.ExitCode,
		CreatedAtMs:     a.CreatedAtMs,
		UpdatedAtMs:     a.UpdatedAtMs,
		SessionId:       a.SessionID,
		TmuxSession:     a.TmuxSession,
		AttachedClients: a.attached,
		Sandbox:         a.sandbox(),
		CloneUrl:        a.CloneURL,
		UsageLimit:      a.limit,
	}
}

// stateDir is the agent's private directory for adapter files.
func (a *agentRec) stateDir() string {
	if a.StateDir != "" {
		return a.StateDir
	}
	return config.Path("agents", a.ID)
}

// sandbox is the agent's sandbox, never SANDBOX_UNSPECIFIED.
func (a *agentRec) sandbox() fleetv1.Sandbox {
	if a.Sandbox == sandboxDocker {
		return sandboxDocker
	}
	return sandboxNone
}

// finish moves a live agent to a final state.
func (a *agentRec) finish(state fleetv1.AgentState, detail string, exit *int) {
	a.State, a.StateDetail, a.attached = state, detail, 0
	if exit != nil {
		a.HasExitCode, a.ExitCode = true, int32(*exit)
	}
}

// subscriber receives events for one connection.
type subscriber struct {
	ch chan *fleetv1.Event
}

// manager owns the agent registry. All fields below mu are guarded by it;
// tmux and git are only called with mu released.
type manager struct {
	d    *daemon
	path string

	mu     sync.Mutex
	agents map[string]*agentRec
	subs   map[*subscriber]struct{}
	// readyGen counts busy->ready transitions (see agentRec.readyGen).
	readyGen uint64

	// hookLns are the hook sockets of sandboxed agents, by agent id.
	hookMu  sync.Mutex
	hookLns map[string]net.Listener
	hookWG  sync.WaitGroup

	// models follow each agent's session for its model and effort, and
	// switching marks agents whose model is being switched (model.go).
	modelMu   sync.Mutex
	models    map[string]*modelCursor
	switching map[string]bool

	// asks are the hooks waiting for answers to questions, by id (asks.go);
	// guarded by mu.
	asks map[string]*pendingAsk

	// limits follow each live agent's session for usage limits (limits.go).
	limitMu sync.Mutex
	limits  map[string]*limitWatch
}

// readyLocked clears a's busy flag and stamps it with a new ready generation.
func (m *manager) readyLocked(a *agentRec) {
	m.readyGen++
	a.busy, a.readyGen = false, m.readyGen
}

func newManager(d *daemon) (*manager, error) {
	m := &manager{
		d:       d,
		path:    config.Path("agents.json"),
		agents:  map[string]*agentRec{},
		subs:    map[*subscriber]struct{}{},
		hookLns: map[string]net.Listener{},
	}
	b, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*agentRec
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	for _, a := range list {
		if a.ID != "" {
			m.agents[a.ID] = a
		}
	}
	return m, nil
}

// sortedLocked returns agents ordered by creation time.
func (m *manager) sortedLocked() []*agentRec {
	out := make([]*agentRec, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAtMs != out[j].CreatedAtMs {
			return out[i].CreatedAtMs < out[j].CreatedAtMs
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// saveLocked writes agents.json atomically.
func (m *manager) saveLocked() {
	b, err := json.MarshalIndent(m.sortedLocked(), "", "  ")
	if err == nil {
		err = writeFileAtomic(m.path, b)
	}
	if err != nil {
		m.d.log.Error("saving agent registry failed", "err", err)
	}
}

// changedLocked persists a and tells subscribers.
func (m *manager) changedLocked(a *agentRec) {
	a.UpdatedAtMs = time.Now().UnixMilli()
	m.saveLocked()
	m.broadcastLocked(&fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a.proto()}})
}

// removeLocked drops an agent from the registry and deletes its state dir.
func (m *manager) removeLocked(a *agentRec) {
	delete(m.agents, a.ID)
	m.dropCursor(a.ID)
	m.saveLocked()
	m.broadcastLocked(&fleetv1.Event{Kind: &fleetv1.Event_AgentRemoved{AgentRemoved: a.ID}})
	if err := os.RemoveAll(a.stateDir()); err != nil {
		m.d.log.Warn("removing agent state dir failed", "agent", a.ID, "err", err)
	}
}

// trimHistoryLocked forgets the oldest finished agents beyond maxHistory.
func (m *manager) trimHistoryLocked() {
	var done []*agentRec
	for _, a := range m.agents {
		if !a.live() && !a.busy {
			done = append(done, a)
		}
	}
	if len(done) <= maxHistory {
		return
	}
	sort.Slice(done, func(i, j int) bool { return done[i].UpdatedAtMs < done[j].UpdatedAtMs })
	for _, a := range done[:len(done)-maxHistory] {
		m.removeLocked(a)
	}
}

// findLocked resolves an agent id or name. Live agents win over finished
// ones with the same name; among finished ones the newest wins.
func (m *manager) findLocked(ref string) *agentRec {
	if a, ok := m.agents[ref]; ok {
		return a
	}
	var best *agentRec
	for _, a := range m.agents {
		if a.Name != ref {
			continue
		}
		if a.live() {
			return a
		}
		if best == nil || a.CreatedAtMs > best.CreatedAtMs {
			best = a
		}
	}
	return best
}

func (m *manager) find(ref string) (*agentRec, error) {
	if ref == "" {
		return nil, errf(codeInvalid, "agent is required")
	}
	a := m.findLocked(ref)
	if a == nil {
		return nil, errf(codeNotFound, "no agent %q", ref)
	}
	return a, nil
}

// list returns agents, optionally including finished ones.
func (m *manager) list(includeFinished bool) []*fleetv1.Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*fleetv1.Agent
	for _, a := range m.sortedLocked() {
		if includeFinished || a.live() {
			out = append(out, a.proto())
		}
	}
	return out
}

// livePaths returns the requested directories of live agents.
func (m *manager) livePaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, a := range m.agents {
		if a.live() {
			out = append(out, a.Path)
		}
	}
	return out
}

// newIDLocked returns an unused 6-hex-digit agent id.
func (m *manager) newIDLocked() string {
	for {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		id := hex.EncodeToString(b[:])
		if _, taken := m.agents[id]; !taken {
			return id
		}
	}
}

// ---------------------------------------------------------------------------
// Events

// subscribe registers a subscriber, pre-filled with the snapshot.
func (m *manager) subscribe() *subscriber {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &subscriber{ch: make(chan *fleetv1.Event, len(m.agents)+subscriberBuffer)}
	for _, a := range m.sortedLocked() {
		s.ch <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a.proto()}}
	}
	s.ch <- &fleetv1.Event{Kind: &fleetv1.Event_SnapshotDone{SnapshotDone: &fleetv1.SnapshotDone{}}}
	m.subs[s] = struct{}{}
	return s
}

// unsubscribe removes s and closes its channel.
func (m *manager) unsubscribe(s *subscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[s]; ok {
		delete(m.subs, s)
		close(s.ch)
	}
}

// resnapshot queues a fresh snapshot plus SnapshotDone for an existing
// subscriber (a repeated SubscribeRequest), ordered with live events.
func (m *manager) resnapshot(s *subscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[s]; !ok {
		return
	}
	for _, a := range m.sortedLocked() {
		if !m.queueLocked(s, &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a.proto()}}) {
			return
		}
	}
	m.queueLocked(s, &fleetv1.Event{Kind: &fleetv1.Event_SnapshotDone{SnapshotDone: &fleetv1.SnapshotDone{}}})
}

// broadcastLocked queues ev for every subscriber.
func (m *manager) broadcastLocked(ev *fleetv1.Event) {
	for s := range m.subs {
		m.queueLocked(s, ev)
	}
}

// queueLocked queues ev for s. A subscriber whose buffer is full is dropped
// (its channel closed) so it never blocks the manager; false is returned.
func (m *manager) queueLocked(s *subscriber, ev *fleetv1.Event) bool {
	select {
	case s.ch <- ev:
		return true
	default:
		delete(m.subs, s)
		close(s.ch)
		m.d.log.Warn("dropping slow subscriber")
		return false
	}
}

func (m *manager) broadcastRoots(roots []*fleetv1.Root) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcastLocked(&fleetv1.Event{Kind: &fleetv1.Event_RootsChanged{RootsChanged: &fleetv1.RootsChanged{Roots: roots}}})
}

// handleSubscribe replies, then streams the snapshot and live events. A
// repeated subscribe on the same connection keeps the one event stream and
// sends a fresh snapshot and SnapshotDone on it.
func (c *conn) handleSubscribe(id uint64) error {
	c.mu.Lock()
	existing := c.sub
	c.mu.Unlock()
	if err := c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Subscribe{Subscribe: &fleetv1.SubscribeResponse{}}}); err != nil {
		return err
	}
	if existing != nil {
		c.d.agents.resnapshot(existing)
		return nil
	}
	s := c.d.agents.subscribe()
	c.mu.Lock()
	c.sub = s
	c.mu.Unlock()
	go func() {
		for ev := range s.ch {
			if err := c.send(0, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Event{Event: ev}}); err != nil {
				c.close()
				return
			}
		}
		// Closed by unsubscribe (connection ending) or because we fell behind.
		c.close()
	}()
	return nil
}

// writeFileAtomic writes data to a 0600 temp file next to path and renames
// it into place.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
