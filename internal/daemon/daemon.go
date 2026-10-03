// Package daemon is the fleet daemon: it owns agent sessions (in tmux),
// serves the fleet protocol on a local Unix socket and on TLS for paired
// remote devices, and advertises itself on the LAN.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/discovery"
	"fleet/internal/identity"
	"fleet/internal/sandbox"
	"fleet/internal/tmux"
	"fleet/internal/web"
)

// Options configures Run.
type Options struct {
	// Adapters available to this daemon.
	Adapters *adapter.Registry
	// Version is the daemon version string.
	Version string
	// FleetBinary is the absolute path of the fleet executable (for hooks).
	FleetBinary string
	// Listen overrides config.Listen when non-empty ("off" disables TCP).
	Listen string
	// NoMDNS disables advertisement regardless of config.
	NoMDNS bool
	// Web overrides config.Web when non-empty ("off" disables the web UI).
	Web string
	// Roots are added to config.toml at startup, before any client can
	// connect (`fleet daemon --root`). Paths must be absolute. A folder
	// that already is a root is left as it is.
	Roots []config.Root
}

// daemon is the state shared by every connection.
type daemon struct {
	opts    Options
	log     *slog.Logger
	started time.Time
	// ctx is Run's context; it ends when the daemon stops.
	ctx context.Context

	cfgMu sync.Mutex
	cfg   *config.Config

	server  *identity.Server
	devices *identity.DeviceStore
	codes   *identity.PairingCodes
	tmux    *tmux.Tmux
	docker  *sandbox.Docker
	agents  *manager

	// sbMu guards the per-sandbox-dir caches: dirs Docker was shown to
	// mount, and the fleet binary copied into each.
	sbMu     sync.Mutex
	sbProbed map[string]bool
	sbFleet  map[string]string

	// usage caches what the providers said about plan limits (usage.go).
	usage usageCache

	connsMu sync.Mutex
	conns   map[*conn]struct{}
	connWG  sync.WaitGroup
	// unauthConns counts TLS connections that have not authenticated yet.
	unauthConns atomic.Int32
}

// Run starts the daemon and blocks until ctx is cancelled. On return, all
// listeners are closed; agents keep running in tmux and are re-adopted by
// the next daemon start.
func Run(ctx context.Context, opts Options) error {
	if opts.Adapters == nil {
		opts.Adapters = adapter.NewRegistry()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.FleetBinary == "" {
		if exe, err := os.Executable(); err == nil {
			opts.FleetBinary = exe
		}
	}
	home, err := config.EnsureHome()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	webAddr := webAddr(opts.Web, cfg.Web)
	if webAddr != "off" {
		if err := web.CheckAddr(webAddr); err != nil {
			return err
		}
	}
	unlock, err := lockPIDFile(config.Path("daemon.pid"))
	if err != nil {
		return err
	}
	defer unlock()

	d := &daemon{
		opts:    opts,
		log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		started: time.Now(),
		cfg:     cfg,
		codes:   identity.NewPairingCodes(),
		ctx:     ctx,
		tmux:    tmux.New(cfg.TmuxSocket),
		docker:  &sandbox.Docker{Binary: cfg.Sandbox.DockerOrDefault()},
		conns:   map[*conn]struct{}{},

		sbProbed: map[string]bool{},
		sbFleet:  map[string]string{},
	}
	// The identity lives in <home>/identity; LoadOrCreateServer appends it.
	if d.server, err = identity.LoadOrCreateServer(home, cfg.Name); err != nil {
		return err
	}
	var ws *web.Server
	wsrc := &webSource{d: d}
	if webAddr != "off" {
		ws, err = web.New(web.Options{
			Addr: webAddr, Source: wsrc, Log: d.log,
			TokenPath: config.Path("web-token"), KeyPath: config.Path("fleet-key"),
			PushKeyPath: config.Path("push-key"), PushSubsPath: config.Path("push.json"),
		})
		if err != nil {
			return err
		}
	}
	if d.devices, err = identity.OpenDeviceStore(config.Path("devices.json")); err != nil {
		return err
	}
	if _, err := d.tmux.Available(ctx); err != nil {
		return fmt.Errorf("tmux is required: %w", err)
	}
	d.checkAuthConfig(*cfg)
	if d.agents, err = newManager(d); err != nil {
		return err
	}
	if err := d.agents.adopt(ctx); err != nil {
		return err
	}
	defer d.agents.closeHookSockets()
	if err := d.ensureRoots(opts.Roots); err != nil {
		return err
	}

	unixLn, err := listenUnix(config.SocketPath())
	if err != nil {
		return err
	}
	lns := []net.Listener{unixLn}
	defer os.Remove(config.SocketPath())

	listen := opts.Listen
	if listen == "" {
		listen = cfg.Listen
	}
	var tlsPort int
	if listen != "" && listen != "off" {
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			unixLn.Close()
			return fmt.Errorf("listen %s: %w", listen, err)
		}
		tlsPort = ln.Addr().(*net.TCPAddr).Port
		lns = append(lns, ln)
		d.log.Info("listening", "tls", ln.Addr().String())
	}
	// The web UI is optional: a busy port must not stop the daemon.
	var webLn net.Listener
	if ws != nil {
		if webLn, err = net.Listen("tcp", webAddr); err != nil {
			d.log.Warn("web ui disabled", "addr", webAddr, "err", err)
			ws = nil
		}
	}
	d.log.Info("fleet daemon started", "version", opts.Version, "socket", config.SocketPath(), "server_id", d.server.ID[:16])

	mdns := tlsPort != 0 && !opts.NoMDNS && cfg.MDNSEnabled()
	if mdns {
		stop, err := discovery.Advertise(ctx, discovery.Advertisement{
			Instance: cfg.Name, ServerID: d.server.ID, Name: cfg.Name, Port: tlsPort,
			WebPort: lanPort(webLn),
		})
		if err != nil {
			d.log.Warn("mdns advertisement failed", "err", err)
			mdns = false // not advertised; the web UI must not claim it is
		} else {
			defer stop()
		}
	}

	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		d.agents.loop(ctx)
	}()
	for i, ln := range lns {
		bg.Add(1)
		go func(ln net.Listener, local bool) {
			defer bg.Done()
			d.serve(ctx, ln, local, "")
		}(ln, i == 0)
	}
	if ws != nil {
		wsrc.listen, wsrc.mdns = listen, mdns
		if listen == "" {
			wsrc.listen = "off"
		}
		bg.Add(1)
		go func() {
			defer bg.Done()
			ws.Serve(ctx, webLn)
		}()
	}

	<-ctx.Done()
	for _, ln := range lns {
		ln.Close()
	}
	d.agents.closeHookSockets()
	d.closeAllConns()
	bg.Wait()
	d.connWG.Wait()
	d.log.Info("fleet daemon stopped")
	return nil
}

// lanPort returns ln's port if other machines can reach it (it does not
// listen on loopback only), else 0.
func lanPort(ln net.Listener) int {
	if ln == nil {
		return 0
	}
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok || a.IP.IsLoopback() {
		return 0
	}
	return a.Port
}

// webAddr resolves the web UI address: the flag, else config, else
// config.DefaultWeb.
func webAddr(flag, cfg string) string {
	switch {
	case flag != "":
		return flag
	case cfg != "":
		return cfg
	}
	return config.DefaultWeb
}

// lockPIDFile takes an exclusive flock on path and writes our pid into it.
// The returned func empties the file and releases the lock.
//
// The file is never unlinked while locked: the flock, not the file's
// existence, marks ownership, and a daemon that locked an already unlinked
// inode would run unseen next to one that created a new file. For the same
// reason a lock on an inode that is no longer at path is retried.
func lockPIDFile(path string) (unlock func(), err error) {
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			b, _ := os.ReadFile(path)
			f.Close()
			if pid := strings.TrimSpace(string(b)); pid != "" {
				return nil, fmt.Errorf("fleet daemon already running (pid %s)", pid)
			}
			return nil, errors.New("fleet daemon already running")
		}
		if !samePath(f, path) {
			f.Close()
			if attempt >= 3 {
				return nil, fmt.Errorf("%s keeps changing; is another daemon starting?", path)
			}
			continue
		}
		if err = f.Truncate(0); err == nil {
			_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		return func() {
			_ = f.Truncate(0)
			f.Close()
		}, nil
	}
}

// samePath reports whether the open file f is still the file at path.
func samePath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, pi)
}

// listenUnix listens on path with mode 0600, replacing a stale socket. The
// caller holds the pid lock, so no other daemon owns path.
func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// config returns a snapshot copy of the config. Code that modifies d.cfg
// holds cfgMu.
func (d *daemon) config() config.Config {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	c := *d.cfg
	c.Roots = append([]config.Root(nil), d.cfg.Roots...)
	return c
}
