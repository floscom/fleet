package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/sandbox"
	"fleet/internal/worktree"
)

const (
	sandboxUnspecified = fleetv1.Sandbox_SANDBOX_UNSPECIFIED
	sandboxNone        = fleetv1.Sandbox_SANDBOX_NONE
	sandboxDocker      = fleetv1.Sandbox_SANDBOX_DOCKER

	// sandboxHome is HOME inside containers; config.Path("sandbox", "home")
	// is mounted there, so logins and CLI settings survive the container.
	sandboxHome = "/home/fleet"
	// hookSocket is the per-agent socket name in the agent's state dir. The
	// state dir doubles as FLEET_HOME inside the container, so `fleet hook`
	// finds it without knowing it is sandboxed.
	hookSocket = "fleet.sock"
	// envFile holds the container environment, in the state dir.
	envFile = "docker.env"
	// dockerTimeout bounds docker calls other than the agent's own `docker run`.
	dockerTimeout = 30 * time.Second
)

// sandboxFor resolves the sandbox a RunAgentRequest asks for.
func sandboxFor(req *fleetv1.RunAgentRequest, cfg config.Config) (fleetv1.Sandbox, error) {
	switch s := req.GetSandbox(); s {
	case sandboxNone, sandboxDocker:
		return s, nil
	case sandboxUnspecified:
		if cfg.Sandbox.Default == "docker" {
			return sandboxDocker, nil
		}
		return sandboxNone, nil
	default:
		return 0, errf(codeInvalid, "unknown sandbox %v", s)
	}
}

// checkDocker fails unless Docker is reachable, the sandbox image exists
// and Docker can mount the sandbox dir. The mount check runs once per dir.
func (d *daemon) checkDocker(ctx context.Context, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	if _, err := d.docker.Version(ctx); err != nil {
		return errf(codeUnavailable, "docker is not available: %v", err)
	}
	image := cfg.Sandbox.ImageOrDefault()
	if err := d.docker.CheckImage(ctx, image); err != nil {
		if errors.Is(err, sandbox.ErrNoImage) && image == config.DefaultSandboxImage {
			return errf(codeUnavailable, "sandbox image %s not found; build it on the server with: fleet sandbox build", image)
		}
		return errf(codeUnavailable, "sandbox image: %v", err)
	}
	dir := cfg.Sandbox.DirOrDefault()
	d.sbMu.Lock()
	defer d.sbMu.Unlock()
	if d.sbProbed[dir] {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := d.docker.Probe(ctx, image, dir); err != nil {
		return errf(codeUnavailable, "docker cannot mount the sandbox dir %s (%v); "+
			"Docker installed as a snap cannot see /tmp or hidden folders such as ~/.fleet: "+
			"set [sandbox] dir in config.toml to a folder it can see, e.g. \"~/fleet-sandbox\", and restart the daemon", dir, err)
	}
	d.sbProbed[dir] = true
	return nil
}

// sandboxFleet returns a copy of the fleet executable in <dir>/bin, named
// after its content, so containers can mount it wherever the daemon's own
// binary lives. Copies of other versions are removed; running containers
// keep the file they mounted.
func (d *daemon) sandboxFleet(dir string) (string, error) {
	d.sbMu.Lock()
	defer d.sbMu.Unlock()
	if p := d.sbFleet[dir]; p != "" && fileExists(p) {
		return p, nil
	}
	data, err := os.ReadFile(d.opts.FleetBinary)
	if err != nil {
		return "", fmt.Errorf("read fleet binary: %w", err)
	}
	sum := sha256.Sum256(data)
	bin := filepath.Join(dir, "bin")
	path := filepath.Join(bin, "fleet-"+hex.EncodeToString(sum[:])[:12])
	if !fileExists(path) {
		if err := os.MkdirAll(bin, 0o700); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(bin, ".fleet-*")
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.Name(), 0o755)
		}
		if err == nil {
			err = os.Rename(f.Name(), path)
		}
		if err != nil {
			os.Remove(f.Name())
			return "", err
		}
	}
	if old, err := filepath.Glob(filepath.Join(bin, "fleet-*")); err == nil {
		for _, p := range old {
			if p != path {
				os.Remove(p)
			}
		}
	}
	d.sbFleet[dir] = path
	return path, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// containerName is "<tmux socket>-<agent id>": "fleet-a1b2c3" for the
// default home, unique per home otherwise (container names are global).
func containerName(cfg config.Config, id string) string {
	return worktree.SanitizeBranch(cfg.TmuxSocket) + "-" + id
}

// dockerLaunch is what launch needs to wrap an agent command in a container.
type dockerLaunch struct {
	a        agentRec
	cwd      string // the agent's working directory
	checkout string // worktree or clone the daemon created; "" when pinned
	stateDir string
	home     string // mounted at sandboxHome
	fleetBin string // copy of the fleet binary for hooks
	spec     *adapter.LaunchSpec
	env      map[string]string
}

// dockerArgv writes the container's env file and returns the `docker run`
// command that runs l.spec in a container.
func (m *manager) dockerArgv(ctx context.Context, l dockerLaunch, cfg config.Config) ([]string, error) {
	d := m.d
	mounts, err := sandboxMounts(ctx, l)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, name := range cfg.Sandbox.EnvOrDefault() {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for k, v := range gitIdentity(ctx) {
		env[k] = v
	}
	for k, v := range l.spec.Env {
		env[k] = v
	}
	for k, v := range l.env {
		env[k] = v
	}
	env["HOME"] = sandboxHome
	env["FLEET_HOME"] = l.stateDir
	env["FLEET_SOCKET"] = filepath.Join(l.stateDir, hookSocket)
	env["TERM"] = "xterm-256color"
	env["COLORTERM"] = "truecolor"
	file := filepath.Join(l.stateDir, envFile)
	skipped, err := sandbox.WriteEnvFile(file, env)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		d.log.Warn("variables with line breaks are not passed into the sandbox", "agent", l.a.ID, "names", skipped)
	}
	return d.docker.RunArgv(sandbox.RunSpec{
		Name:  l.a.Container,
		Image: cfg.Sandbox.ImageOrDefault(),
		Labels: map[string]string{
			sandbox.LabelAgent: l.a.ID,
			sandbox.LabelHome:  config.Path(),
		},
		User:      strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Workdir:   l.cwd,
		Mounts:    mounts,
		EnvFile:   file,
		Network:   cfg.Sandbox.Network,
		ExtraArgs: cfg.Sandbox.Args,
		Argv:      l.spec.Argv,
	}), nil
}

// sandboxMounts decides what the container sees, each at its host path:
//
//   - the checkout: the worktree or clone, or for a pinned agent its folder
//     (the whole repository when the folder is inside one), read-write;
//   - the repository's shared git directory when it lies outside the
//     checkout (worktrees), read-write so the agent can commit;
//   - the git config and hooks directory, and a worktree's .git file,
//     read-only: git on the host executes what they name;
//   - the agent's state dir (generated settings and the hook socket) and
//     the fleet binary, read-only;
//   - the sandbox home at sandboxHome.
func sandboxMounts(ctx context.Context, l dockerLaunch) ([]sandbox.Mount, error) {
	work := l.checkout
	if work == "" {
		work = l.cwd
		if top, ok := worktree.RepoRoot(ctx, l.cwd); ok {
			work = top
		}
	}
	if err := os.MkdirAll(l.home, 0o700); err != nil {
		return nil, err
	}
	mounts := []sandbox.Mount{
		{Source: work, Target: work},
		ro(l.stateDir),
		{Source: l.home, Target: sandboxHome},
		ro(l.fleetBin),
	}
	if _, ok := worktree.RepoRoot(ctx, work); !ok {
		return mounts, nil
	}
	common, err := worktree.GitCommonDir(ctx, work)
	if err != nil {
		return nil, err
	}
	if !within(work, common) {
		mounts = append(mounts, sandbox.Mount{Source: common, Target: common})
	}
	if fi, err := os.Lstat(filepath.Join(work, ".git")); err == nil && fi.Mode().IsRegular() {
		mounts = append(mounts, ro(filepath.Join(work, ".git")))
	}
	if fi, err := os.Stat(filepath.Join(common, "config")); err == nil && fi.Mode().IsRegular() {
		mounts = append(mounts, ro(filepath.Join(common, "config")))
	}
	hooks := filepath.Join(common, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return nil, err
	}
	return append(mounts, ro(hooks)), nil
}

func ro(p string) sandbox.Mount { return sandbox.Mount{Source: p, Target: p, ReadOnly: true} }

// gitIdentity passes the daemon user's git identity into the container,
// which has no access to ~/.gitconfig.
func gitIdentity(ctx context.Context) map[string]string {
	env := map[string]string{}
	for key, names := range map[string][]string{
		"user.name":  {"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"},
		"user.email": {"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"},
	} {
		out, err := exec.CommandContext(ctx, "git", "config", "--global", "--get", key).Output()
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			for _, n := range names {
				env[n] = v
			}
		}
	}
	return env
}

// ---------------------------------------------------------------------------
// Hook sockets

// openHookSocket listens on the agent's hook socket. Connections on it can
// only deliver hook events, and only for that agent: it is all a sandboxed
// agent gets of the daemon.
func (m *manager) openHookSocket(id, stateDir string) error {
	path := filepath.Join(stateDir, hookSocket)
	ln, err := listenUnix(path)
	if err != nil {
		return err
	}
	m.hookMu.Lock()
	if old := m.hookLns[id]; old != nil {
		old.Close()
	}
	m.hookLns[id] = ln
	m.hookMu.Unlock()
	m.hookWG.Add(1)
	go func() {
		defer m.hookWG.Done()
		m.d.serve(m.d.ctx, ln, true, id)
	}()
	return nil
}

// closeHookSocket stops an agent's hook socket, if it has one, and removes
// the socket file.
func (m *manager) closeHookSocket(id string) {
	m.hookMu.Lock()
	ln := m.hookLns[id]
	delete(m.hookLns, id)
	m.hookMu.Unlock()
	if ln != nil {
		ln.Close()
		os.Remove(ln.Addr().String())
	}
}

// closeHookSockets stops every hook socket and waits for their accept loops.
func (m *manager) closeHookSockets() {
	m.hookMu.Lock()
	lns := m.hookLns
	m.hookLns = map[string]net.Listener{}
	m.hookMu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	m.hookWG.Wait()
}

// releaseSandbox tears down what a finished sandboxed agent leaves behind:
// its hook socket and its container. Killing the docker client (with the
// tmux session) does not stop the container, so it is removed explicitly.
// A login the agent refreshed is synced back to the host.
func (m *manager) releaseSandbox(ctx context.Context, id, container string) {
	m.closeHookSocket(id)
	if container == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerTimeout)
	defer cancel()
	if err := m.d.docker.Remove(ctx, container); err != nil {
		m.d.log.Warn("removing container failed", "agent", id, "container", container, "err", err)
	}
	m.syncAuth(ctx, m.d.config())
}

// reapContainers removes containers of this daemon home that belong to no
// live agent. Runs at startup, like the tmux orphan sweep.
func (m *manager) reapContainers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	names, err := m.d.docker.Containers(ctx, sandbox.LabelHome, config.Path())
	if err != nil {
		if _, lerr := exec.LookPath(m.d.docker.Binary); lerr == nil {
			m.d.log.Warn("listing sandbox containers failed", "err", err)
		}
		return
	}
	m.mu.Lock()
	owned := map[string]bool{}
	for _, a := range m.agents {
		if a.live() && a.Container != "" {
			owned[a.Container] = true
		}
	}
	m.mu.Unlock()
	for _, n := range names {
		if owned[n] {
			continue
		}
		m.d.log.Warn("removing orphan sandbox container", "container", n)
		if err := m.d.docker.Remove(ctx, n); err != nil {
			m.d.log.Warn("removing orphan container failed", "container", n, "err", err)
		}
	}
}

// removeClone deletes a clone unless it has uncommitted changes or commits
// that were never pushed (and force is not set). It reports whether the
// clone was kept, and why.
func removeClone(ctx context.Context, path string, force bool) (kept bool, reason string) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, ""
	}
	if !force {
		if dirty, err := worktree.Dirty(ctx, path); err != nil {
			return true, "cannot check clone status: " + err.Error()
		} else if dirty {
			return true, "clone has uncommitted changes (use force to remove it anyway)"
		}
		if unpushed, err := worktree.Unpushed(ctx, path); err != nil {
			return true, "cannot check for unpushed commits: " + err.Error()
		} else if unpushed {
			return true, "clone has commits that are on no remote branch (use force to remove it anyway)"
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return true, fmt.Sprintf("remove clone: %v", err)
	}
	return false, ""
}
