package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/tmux"
	"fleet/internal/worktree"
)

const (
	defaultCols = 200
	defaultRows = 50
	maxDim      = 1000
	// launchTimeout bounds worktree creation plus session start. Launching
	// is detached from the request so a disconnecting client cannot leave a
	// half-created agent behind.
	launchTimeout = 60 * time.Second
)

const (
	isoUnspecified = fleetv1.Isolation_ISOLATION_UNSPECIFIED
	isoPinned      = fleetv1.Isolation_ISOLATION_PINNED
	isoWorktree    = fleetv1.Isolation_ISOLATION_WORKTREE

	stateStarting = fleetv1.AgentState_AGENT_STATE_STARTING
	stateRunning  = fleetv1.AgentState_AGENT_STATE_RUNNING
	stateExited   = fleetv1.AgentState_AGENT_STATE_EXITED
	stateFailed   = fleetv1.AgentState_AGENT_STATE_FAILED
)

// dim returns v, or def when v is 0, capped at maxDim.
func dim(v uint32, def int) int {
	if v == 0 {
		return def
	}
	return min(int(v), maxDim)
}

// run validates a RunAgentRequest, reserves the agent in the registry, then
// creates its worktree (if any) and tmux session.
func (m *manager) run(ctx context.Context, req *fleetv1.RunAgentRequest) (*fleetv1.Agent, error) {
	d := m.d
	ad, ok := d.opts.Adapters.Get(req.GetAdapter())
	if !ok {
		return nil, errf(codeNotFound, "unknown adapter %q", req.GetAdapter())
	}
	if det := ad.Detect(ctx); !det.Available {
		return nil, errf(codeUnavailable, "%s is not available: %s", ad.DisplayName(), det.Reason)
	}
	if req.GetRoot() == "" && req.GetAbsolutePath() == "" {
		return nil, errf(codeInvalid, "root and path, or absolute_path, is required")
	}
	cfg := d.config()
	root, dir, err := cfg.Resolve(req.GetRoot(), req.GetPath(), req.GetAbsolutePath())
	if err != nil {
		return nil, resolveErr(err)
	}
	if len(root.Adapters) > 0 && !slices.Contains(root.Adapters, ad.ID()) {
		return nil, errf(codeDenied, "adapter %q is not allowed in root %q", ad.ID(), root.Name)
	}

	repo, inRepo := worktree.RepoRoot(ctx, dir)
	iso := req.GetIsolation()
	switch iso {
	case isoUnspecified:
		iso = isoPinned
		if inRepo && cfg.DefaultIsolation != "pinned" {
			iso = isoWorktree
		}
	case isoWorktree:
		if !inRepo {
			return nil, errf(codeInvalid, "worktree isolation needs a git repository; %s is not in one", dir)
		}
	case isoPinned:
	default:
		return nil, errf(codeInvalid, "unknown isolation %v", iso)
	}
	name := clean(req.GetName(), maxNameLen)
	if req.GetName() != "" && name == "" {
		return nil, errf(codeInvalid, "invalid agent name %q", req.GetName())
	}

	// Reserve the agent so concurrent requests see its name and directory.
	m.mu.Lock()
	for _, o := range m.agents {
		if !o.live() {
			continue
		}
		if name != "" && (o.Name == name || o.ID == name) {
			m.mu.Unlock()
			return nil, errf(codeExists, "an agent named %q is already running", name)
		}
		if iso == isoPinned && o.Isolation == isoPinned && o.Cwd == dir {
			m.mu.Unlock()
			return nil, errf(codeBusy, "agent %q is already pinned to %s", o.Name, dir)
		}
	}
	if name == "" {
		name = m.genNameLocked(ad.ID(), dir)
	}
	id := m.newIDLocked()
	now := time.Now().UnixMilli()
	a := &agentRec{
		ID: id, Name: name, Adapter: ad.ID(), Path: dir, Root: root.Name, Cwd: dir,
		Isolation: iso, State: stateStarting, CreatedAtMs: now,
		TmuxSession: tmux.SessionName(id), busy: true,
	}
	if iso == isoWorktree {
		a.Branch = req.GetBranch()
		if a.Branch == "" {
			a.Branch = "fleet/" + worktree.SanitizeBranch(name)
		}
	}
	m.agents[id] = a
	m.changedLocked(a)
	rec := *a
	m.mu.Unlock()

	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchTimeout)
	defer cancel()
	res, err := m.launch(lctx, ad, rec, req, repo, cfg)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyLocked(a)
	if err != nil {
		m.removeLocked(a)
		d.log.Warn("agent failed to start", "agent", id, "adapter", ad.ID(), "err", err)
		return nil, err
	}
	a.Cwd, a.Worktree, a.SessionID = res.cwd, res.worktree, res.sessionID
	a.StartedAtMs = time.Now().UnixMilli()
	if a.State == stateStarting { // a hook may already have reported more
		a.State = stateRunning
	}
	m.changedLocked(a)
	d.log.Info("agent started", "agent", id, "name", name, "adapter", ad.ID(), "cwd", a.Cwd)
	return a.proto(), nil
}

type launchResult struct {
	cwd, worktree, sessionID string
}

// launch creates the worktree and the tmux session for a reserved agent,
// undoing the worktree on failure.
func (m *manager) launch(ctx context.Context, ad adapter.Adapter, a agentRec, req *fleetv1.RunAgentRequest, repo string, cfg config.Config) (res launchResult, err error) {
	d := m.d
	res.cwd = a.Cwd
	stateDir := config.Path("agents", a.ID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return res, err
	}
	if a.Isolation == isoWorktree {
		wt := worktreeDir(repo, a.Name, a.ID)
		if err := worktree.Add(ctx, worktree.AddOptions{Repo: repo, Path: wt, Branch: a.Branch}); err != nil {
			return res, errf(codeInvalid, "create worktree: %v", err)
		}
		res.worktree = wt
		defer func() {
			if err != nil {
				if rerr := worktree.Remove(context.WithoutCancel(ctx), wt, true); rerr != nil {
					d.log.Warn("removing worktree after failed start", "path", wt, "err", rerr)
				}
			}
		}()
		cwd, err := worktree.SubdirIn(repo, a.Path, wt)
		if err != nil {
			return res, err
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			// The subdirectory holds no tracked files, so the checkout lacks it.
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				return res, err
			}
		}
		res.cwd = cwd
	}

	extra := append(slices.Clone(cfg.Adapters[ad.ID()].Args), req.GetExtraArgs()...)
	spec, err := ad.Launch(ctx, adapter.LaunchRequest{
		AgentID:     a.ID,
		AgentName:   a.Name,
		Cwd:         res.cwd,
		Prompt:      req.GetPrompt(),
		ExtraArgs:   extra,
		FleetBinary: d.opts.FleetBinary,
		StateDir:    stateDir,
	})
	if err != nil {
		return res, errf(codeUnavailable, "%s: %v", ad.ID(), err)
	}
	if len(spec.Argv) == 0 {
		return res, fmt.Errorf("adapter %s returned an empty command", ad.ID())
	}
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["FLEET_AGENT_ID"] = a.ID
	env["FLEET_SOCKET"] = config.SocketPath()
	env["FLEET_HOME"] = config.Path()
	err = d.tmux.NewSession(ctx, tmux.NewSessionOptions{
		Name:     a.TmuxSession,
		Cwd:      res.cwd,
		Argv:     spec.Argv,
		Env:      env,
		UnsetEnv: spec.UnsetEnv,
		Cols:     dim(req.GetCols(), defaultCols),
		Rows:     dim(req.GetRows(), defaultRows),
	})
	if err != nil {
		return res, err
	}
	res.sessionID = spec.SessionID
	return res, nil
}

// worktreeDir is <home>/worktrees/<repo base>-<hash>/<agent name>, with the
// agent id appended if a kept worktree already occupies that path.
func worktreeDir(repo, name, id string) string {
	sum := sha256.Sum256([]byte(repo))
	dir := config.Path("worktrees", filepath.Base(repo)+"-"+hex.EncodeToString(sum[:])[:8], worktree.SanitizeBranch(name))
	if _, err := os.Lstat(dir); err == nil {
		dir += "-" + id
	}
	return dir
}

// genNameLocked returns "<adapter>-<dir base>-<n>" not used by any agent.
func (m *manager) genNameLocked(adapterID, dir string) string {
	base := adapterID + "-" + worktree.SanitizeBranch(filepath.Base(dir))
	for n := 1; ; n++ {
		cand := base + "-" + strconv.Itoa(n)
		taken := false
		for _, a := range m.agents {
			if a.Name == cand {
				taken = true
				break
			}
		}
		if !taken {
			return cand
		}
	}
}

// kill stops an agent's session, then optionally removes its worktree and
// forgets it.
func (m *manager) kill(ctx context.Context, req *fleetv1.KillAgentRequest) (*fleetv1.KillAgentResponse, error) {
	d := m.d
	m.mu.Lock()
	a, err := m.find(req.GetAgent())
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if a.busy {
		m.mu.Unlock()
		return nil, errf(codeInvalid, "agent %s is busy (starting or being killed)", a.ID)
	}
	live, session := a.live(), a.TmuxSession
	a.busy = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.readyLocked(a)
		m.mu.Unlock()
	}()

	if live {
		var dead *tmux.PaneStatus
		if panes, err := d.tmux.List(ctx); err == nil {
			for _, p := range panes {
				if p.Session == session && p.Dead {
					dead = &p
				}
			}
		}
		if err := d.tmux.KillSession(ctx, session); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if a.live() {
			detail, exit := "killed", (*int)(nil)
			if dead != nil {
				detail, exit = dead.Detail(), &dead.ExitStatus
			}
			a.finish(stateExited, detail, exit)
			m.changedLocked(a)
		}
		m.mu.Unlock()
		d.log.Info("agent killed", "agent", a.ID)
	}

	resp := &fleetv1.KillAgentResponse{}
	m.mu.Lock()
	wt := ""
	if a.Worktree != "" && !a.WorktreeRemoved {
		wt = a.Worktree
	}
	m.mu.Unlock()
	if req.GetRemoveWorktree() && wt != "" {
		resp.WorktreeKept, resp.WorktreeKeptReason = removeWorktree(ctx, wt, req.GetForce())
		if !resp.WorktreeKept {
			d.log.Info("worktree removed", "agent", a.ID, "path", wt)
			m.mu.Lock()
			a.WorktreeRemoved = true
			m.changedLocked(a)
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	resp.Agent = a.proto()
	if req.GetForget() {
		if _, ok := m.agents[a.ID]; ok {
			m.removeLocked(a)
		}
	} else {
		m.trimHistoryLocked()
	}
	return resp, nil
}

// removeWorktree removes a worktree unless it has uncommitted changes (and
// force is not set). It reports whether the worktree was kept, and why.
func removeWorktree(ctx context.Context, path string, force bool) (kept bool, reason string) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, ""
	}
	if !force {
		dirty, err := worktree.Dirty(ctx, path)
		if err != nil {
			return true, "cannot check worktree status: " + err.Error()
		}
		if dirty {
			return true, "worktree has uncommitted changes (use force to remove it anyway)"
		}
	}
	if err := worktree.Remove(ctx, path, force); err != nil {
		return true, err.Error()
	}
	return false, ""
}

// sendText types into a live agent's terminal.
func (m *manager) sendText(ctx context.Context, req *fleetv1.SendTextRequest) error {
	m.mu.Lock()
	a, err := m.find(req.GetAgent())
	var session string
	if err == nil {
		if !a.live() {
			err = errf(codeInvalid, "agent %s is not running", a.ID)
		}
		session = a.TmuxSession
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.d.tmux.SendText(ctx, session, req.GetText(), req.GetSubmit())
}
