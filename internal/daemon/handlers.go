package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/worktree"
)

// maxBrowseEntries caps a BrowseResponse.
const maxBrowseEntries = 2000

// errClose is returned by a handler to close the connection after replying.
var errClose = errors.New("close connection")

// handle processes one client message. It returns false when the connection
// must be closed.
func (c *conn) handle(m *fleetv1.ClientMessage) (keep bool) {
	id := m.GetId()
	defer func() {
		if r := recover(); r != nil {
			c.d.log.Error("panic in handler", "panic", r, "stack", string(debug.Stack()))
			keep = c.sendError(id, codeInternal, "internal error") == nil
		}
	}()
	err := c.dispatch(id, m)
	if err == nil {
		return true
	}
	if errors.Is(err, errClose) {
		return false
	}
	if id == 0 {
		// Fire-and-forget messages (TerminalInput, TerminalResize) get no
		// reply: the client has no request to match an Error to.
		c.d.log.Debug("dropped message failed", "request", fmt.Sprintf("%T", m.GetMsg()), "err", err)
		return true
	}
	code, msg := errorCode(err)
	if code == codeInternal {
		c.d.log.Warn("request failed", "request", fmt.Sprintf("%T", m.GetMsg()), "err", err)
	}
	return c.sendError(id, code, msg) == nil
}

// dispatch enforces authentication and routes to the handler, which sends
// its own reply. A returned error is sent as an Error reply.
func (c *conn) dispatch(id uint64, m *fleetv1.ClientMessage) error {
	if c.hookAgent != "" {
		switch msg := m.GetMsg().(type) {
		case *fleetv1.ClientMessage_Ping:
		case *fleetv1.ClientMessage_Hook:
			msg.Hook.AgentId = c.hookAgent
		default:
			return errf(codeDenied, "this socket only accepts hook events")
		}
	}
	switch msg := m.GetMsg().(type) {
	case *fleetv1.ClientMessage_Ping:
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Pong{Pong: &fleetv1.Pong{}}})
	case *fleetv1.ClientMessage_Pair:
		return c.handlePair(id, msg.Pair)
	case *fleetv1.ClientMessage_Auth:
		return c.handleAuth(id, msg.Auth)
	case nil:
		return errf(codeInvalid, "empty message")
	}

	authed, deviceID := c.identity()
	if !authed {
		return errf(codeUnauth, "authenticate or pair first")
	}
	if deviceID != "" {
		// The device may have been revoked while this connection was
		// authenticating, after handleRevoke looked for its connections.
		if _, ok := c.d.devices.Get(deviceID); !ok {
			if id != 0 {
				c.sendError(id, codeUnauth, "device revoked")
			}
			return errClose
		}
		c.d.devices.Touch(deviceID, time.Now())
	}
	d := c.d
	switch msg := m.GetMsg().(type) {
	case *fleetv1.ClientMessage_GetInfo:
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_GetInfo{GetInfo: d.info()}})
	case *fleetv1.ClientMessage_ListAdapters:
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_ListAdapters{ListAdapters: d.listAdapters(c.ctx)}})
	case *fleetv1.ClientMessage_ListRoots:
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_ListRoots{ListRoots: &fleetv1.ListRootsResponse{Roots: d.roots()}}})
	case *fleetv1.ClientMessage_AddRoot:
		r, err := d.addRoot(msg.AddRoot)
		if err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_AddRoot{AddRoot: &fleetv1.AddRootResponse{Root: r}}})
	case *fleetv1.ClientMessage_RemoveRoot:
		if err := d.removeRoot(msg.RemoveRoot.GetName()); err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_RemoveRoot{RemoveRoot: &fleetv1.RemoveRootResponse{}}})
	case *fleetv1.ClientMessage_Browse:
		r, err := d.browse(msg.Browse)
		if err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Browse{Browse: r}})

	case *fleetv1.ClientMessage_ListAgents:
		agents := d.agents.list(msg.ListAgents.GetIncludeFinished())
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_ListAgents{ListAgents: &fleetv1.ListAgentsResponse{Agents: agents}}})
	case *fleetv1.ClientMessage_RunAgent:
		a, err := d.agents.run(c.ctx, msg.RunAgent)
		if err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_RunAgent{RunAgent: &fleetv1.RunAgentResponse{Agent: a}}})
	case *fleetv1.ClientMessage_KillAgent:
		r, err := d.agents.kill(c.ctx, msg.KillAgent)
		if err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_KillAgent{KillAgent: r}})
	case *fleetv1.ClientMessage_SendText:
		if err := d.agents.sendText(c.ctx, msg.SendText); err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_SendText{SendText: &fleetv1.SendTextResponse{}}})
	case *fleetv1.ClientMessage_Subscribe:
		return c.handleSubscribe(id)

	case *fleetv1.ClientMessage_Attach:
		return c.handleAttach(id, msg.Attach)
	case *fleetv1.ClientMessage_TerminalInput:
		return c.handleInput(id, msg.TerminalInput)
	case *fleetv1.ClientMessage_TerminalResize:
		return c.handleResize(id, msg.TerminalResize)
	case *fleetv1.ClientMessage_Detach:
		return c.handleDetach(id, msg.Detach)

	case *fleetv1.ClientMessage_CreatePairingCode:
		return c.handleCreatePairingCode(id, msg.CreatePairingCode)
	case *fleetv1.ClientMessage_ListDevices:
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_ListDevices{ListDevices: &fleetv1.ListDevicesResponse{Devices: d.listDevices()}}})
	case *fleetv1.ClientMessage_RevokeDevice:
		return c.handleRevoke(id, msg.RevokeDevice)

	case *fleetv1.ClientMessage_Hook:
		if !c.local {
			return errf(codeUnauth, "hook events are only accepted on the local socket")
		}
		if err := d.agents.hook(msg.Hook); err != nil {
			return err
		}
		return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Hook{Hook: &fleetv1.HookResponse{}}})
	}
	return errf(codeInvalid, "unsupported message %T", m.GetMsg())
}

// reply sends a response with the request id. A write failure closes the
// connection.
func (c *conn) reply(id uint64, m *fleetv1.ServerMessage) error {
	if err := c.send(id, m); err != nil {
		return errClose
	}
	return nil
}

func (d *daemon) info() *fleetv1.GetInfoResponse {
	host, _ := os.Hostname()
	return &fleetv1.GetInfoResponse{
		ServerId:      d.server.ID,
		ServerName:    d.config().Name,
		DaemonVersion: d.opts.Version,
		Hostname:      host,
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		StartedAtMs:   d.started.UnixMilli(),
	}
}

// ---------------------------------------------------------------------------
// Roots and browsing

func (d *daemon) roots() []*fleetv1.Root {
	cfg := d.config()
	out := make([]*fleetv1.Root, 0, len(cfg.Roots))
	for _, r := range cfg.Roots {
		out = append(out, r.Proto())
	}
	return out
}

func (d *daemon) addRoot(req *fleetv1.AddRootRequest) (*fleetv1.Root, error) {
	if !filepath.IsAbs(req.GetPath()) {
		return nil, errf(codeInvalid, "root path must be absolute: %q", req.GetPath())
	}
	for _, a := range req.GetAdapters() {
		if _, ok := d.opts.Adapters.Get(a); !ok {
			return nil, errf(codeNotFound, "unknown adapter %q", a)
		}
	}
	d.cfgMu.Lock()
	r, err := d.cfg.AddRoot(config.Root{Path: req.GetPath(), Name: req.GetName(), Adapters: req.GetAdapters(), Trust: req.GetTrust()})
	if err == nil {
		if err = d.cfg.Save(); err != nil {
			_ = d.cfg.RemoveRoot(r.Name)
			err = fmt.Errorf("save config: %w", err)
			d.cfgMu.Unlock()
			return nil, err
		}
	}
	d.cfgMu.Unlock()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errf(codeNotFound, "%v", err)
		}
		return nil, errf(codeInvalid, "%v", err)
	}
	d.log.Info("root added", "name", r.Name, "path", r.Path)
	d.agents.broadcastRoots(d.roots())
	return r.Proto(), nil
}

func (d *daemon) removeRoot(name string) error {
	d.cfgMu.Lock()
	old := append([]config.Root(nil), d.cfg.Roots...)
	if err := d.cfg.RemoveRoot(name); err != nil {
		d.cfgMu.Unlock()
		return resolveErr(err)
	}
	if err := d.cfg.Save(); err != nil {
		d.cfg.Roots = old
		d.cfgMu.Unlock()
		return fmt.Errorf("save config: %w", err)
	}
	d.cfgMu.Unlock()
	d.log.Info("root removed", "name", name)
	d.agents.broadcastRoots(d.roots())
	return nil
}

// resolveErr maps config.Resolve errors to protocol errors.
func resolveErr(err error) error {
	switch {
	case errors.Is(err, config.ErrOutsideRoots):
		return errf(codeOutside, "%v", err)
	case errors.Is(err, config.ErrUnknownRoot), errors.Is(err, fs.ErrNotExist):
		return errf(codeNotFound, "%v", err)
	default:
		return errf(codeInvalid, "%v", err)
	}
}

func (d *daemon) browse(req *fleetv1.BrowseRequest) (*fleetv1.BrowseResponse, error) {
	if req.GetRoot() == "" {
		return nil, errf(codeInvalid, "root is required")
	}
	cfg := d.config()
	root, dir, err := cfg.Resolve(req.GetRoot(), req.GetPath(), "")
	if err != nil {
		return nil, resolveErr(err)
	}
	rootReal, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		return nil, resolveErr(err)
	}
	rel, err := filepath.Rel(rootReal, dir)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		rel = ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, resolveErr(err)
	}
	live := d.agents.livePaths()
	resp := &fleetv1.BrowseResponse{Root: root.Name, Path: filepath.ToSlash(rel)}
	for _, e := range ents {
		name := e.Name()
		if !req.GetIncludeHidden() && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			// Symlinked directories are listed only if they stay inside the root.
			target, err := filepath.EvalSymlinks(full)
			if err != nil || !within(rootReal, target) {
				continue
			}
			if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
				continue
			}
			full = target
		}
		de := &fleetv1.DirEntry{Name: name}
		// IsRepoRoot short-circuits on a .git entry; checking it first avoids
		// spawning git for every plain directory.
		if _, err := os.Lstat(filepath.Join(full, ".git")); err == nil {
			de.IsGitRepo = worktree.IsRepoRoot(full)
		}
		for _, p := range live {
			if within(full, p) {
				de.AgentCount++
			}
		}
		resp.Entries = append(resp.Entries, de)
	}
	sort.Slice(resp.Entries, func(i, j int) bool { return resp.Entries[i].Name < resp.Entries[j].Name })
	if len(resp.Entries) > maxBrowseEntries {
		resp.Entries = resp.Entries[:maxBrowseEntries]
	}
	return resp, nil
}

// within reports whether target equals root or lies below it.
func within(root, target string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
