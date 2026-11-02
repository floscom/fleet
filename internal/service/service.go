// Package service installs the fleet daemon as a service of the OS: a
// systemd unit on Linux, a launchd job on macOS. Run as a normal user it
// is a per-user service (systemd --user, ~/Library/LaunchAgents); run as
// root, a system one (/etc/systemd/system, /Library/LaunchDaemons).
//
// The service runs `fleet daemon` with the flags given at install, logs
// to <FLEET_HOME>/daemon.log like `fleet start`, and is restarted when it
// crashes but not after a clean stop. It never kills the tmux server the
// daemon started, so stopping or restarting it leaves the agents running.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"fleet/internal/config"
)

// ErrUnsupported is returned on an OS without a supported service manager.
var ErrUnsupported = errors.New("services are only supported on Linux (systemd) and macOS (launchd)")

// Spec is what the service runs.
type Spec struct {
	// Binary is the absolute path of the fleet binary.
	Binary string
	// Args follow "daemon", e.g. --listen off.
	Args []string
	// Env is set for the daemon and, through tmux, for every agent.
	Env map[string]string
	// Log receives the daemon's stdout and stderr.
	Log string
}

// Status is what the service manager says about the service.
type Status struct {
	// Installed: the unit file exists.
	Installed bool
	// Enabled: it starts at boot or login.
	Enabled bool
	// Running: the daemon process runs.
	Running bool
	// Pid of the daemon, 0 if unknown.
	Pid int
	// Detail is the manager's own word for the state, e.g. "failed".
	Detail string
}

// Manager installs and drives the fleet service.
type Manager interface {
	// Kind names the manager and scope, e.g. "systemd user unit".
	Kind() string
	// Path is the unit file or plist.
	Path() string
	// Install writes the unit, enables it and (re)starts it. The notes
	// say what else it did or could not do, for the user.
	Install(Spec) (notes []string, err error)
	// Uninstall stops and disables the service and removes the unit.
	Uninstall() error
	// Start starts the installed service.
	Start() error
	// Stop stops the service until the next Start, boot or login.
	Stop() error
	Status() (Status, error)
}

// runCmd runs an external command and returns its combined output. Tests
// replace it.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// run is runCmd with the output folded into the error.
func run(name string, args ...string) (string, error) {
	out, err := runCmd(name, args...)
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, s)
		}
		return s, fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return s, nil
}

// system reports whether this process installs system services (root).
func system() bool { return os.Geteuid() == 0 }

// New returns the manager for this OS, for the user (or root) running fleet
// and for FLEET_HOME: a FLEET_HOME other than ~/.fleet gets its own
// service, named like its tmux socket.
func New() (Manager, error) {
	name := config.DefaultTmuxSocket()
	switch runtime.GOOS {
	case "linux":
		dir := "/etc/systemd/system"
		if !system() {
			cfg, err := os.UserConfigDir()
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(cfg, "systemd", "user")
		}
		return &systemd{name: name, dir: dir, system: system()}, nil
	case "darwin":
		dir, domain := "/Library/LaunchDaemons", "system"
		if !system() {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dir, domain = filepath.Join(home, "Library", "LaunchAgents"), fmt.Sprintf("gui/%d", os.Getuid())
		}
		return &launchd{label: "com.floscom." + name, dir: dir, domain: domain, system: system()}, nil
	}
	return nil, ErrUnsupported
}

// envKeys are taken from the installing shell into the service: PATH so
// the daemon finds tmux, git and the agent CLIs where the shell does (a
// service otherwise gets a minimal one), the locale so tmux draws UTF-8,
// and HOME and FLEET_HOME so the service uses the same home (system
// services get no HOME).
var envKeys = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "HOME", "FLEET_HOME"}

// CurrentEnv returns the envKeys that are set in this process. FLEET_HOME
// is made absolute.
func CurrentEnv() map[string]string {
	env := map[string]string{}
	for _, k := range envKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	if _, ok := env["FLEET_HOME"]; ok {
		env["FLEET_HOME"] = config.Path()
	}
	if p, ok := env["PATH"]; ok {
		env["PATH"] = cleanPath(p)
	}
	return env
}

// cleanPath drops the entries of a PATH that are repeated, relative or not
// folders (shells and agent CLIs pile those up), keeping the order.
func cleanPath(path string) string {
	seen := map[string]bool{}
	var keep []string
	for _, dir := range filepath.SplitList(path) {
		dir = filepath.Clean(dir)
		if seen[dir] || !filepath.IsAbs(dir) {
			continue
		}
		seen[dir] = true
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			keep = append(keep, dir)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// sortedKeys returns m's keys in order, so rendered units are stable.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeFile writes data to path atomically, creating its folder.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
