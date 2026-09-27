// Package config owns fleet's on-disk layout and config.toml.
//
// Layout (FLEET_HOME, default ~/.fleet, mode 0700):
//
//	config.toml        user config (roots, listen and web addresses, name)
//	fleet.sock         local control socket (mode 0600)
//	daemon.pid         pid of the running daemon
//	daemon.log         daemon log when started via `fleet start`
//	identity/          server TLS key+cert, client device key (0600)
//	devices.json       paired devices (0600)
//	web-token          web dashboard admin token (0600, see web.LoadOrCreateToken)
//	servers.json       daemons this machine has paired with as a client (0600)
//	agents.json        agent registry (0600)
//	agents/<id>/       per-agent state dir (adapter files)
//	worktrees/         git worktrees created for agents
//	clones/            repositories cloned for agents (CLONE isolation)
//	sandbox/           what Docker sandboxes mount ([sandbox] dir, see SandboxConfig)
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	fleetv1 "fleet/gen/fleetv1"

	"github.com/BurntSushi/toml"
)

// DefaultListen is the default TCP listen address for remote clients.
const DefaultListen = "0.0.0.0:7420"

// DefaultWeb is the default address of the web dashboard.
const DefaultWeb = "0.0.0.0:7421"

// Config is config.toml.
type Config struct {
	// Name shown to clients and in mDNS. Default: hostname.
	Name string `toml:"name"`
	// Listen is the TCP address for remote (TLS) clients. "" or "off" disables it.
	Listen string `toml:"listen"`
	// Web is the address of the web dashboard (plain HTTP; viewing needs
	// no auth, managing roots needs the web-token). "" means DefaultWeb,
	// "off" disables it.
	Web string `toml:"web,omitempty"`
	// MDNS enables LAN advertisement. Default true.
	MDNS *bool `toml:"mdns"`
	// TmuxSocket is the tmux -L socket name. Default: DefaultTmuxSocket.
	TmuxSocket string `toml:"tmux_socket"`
	// DefaultIsolation: "worktree" (default; falls back to pinned outside git) or "pinned".
	DefaultIsolation string `toml:"default_isolation"`
	Roots            []Root `toml:"root"`
	// Adapter-specific overrides, e.g. [adapter.claude] binary = "/opt/claude".
	Adapters map[string]AdapterConfig `toml:"adapter"`
	// Sandbox configures Docker sandboxes ([sandbox]).
	Sandbox SandboxConfig `toml:"sandbox,omitempty"`
}

// DefaultSandboxImage is the image `fleet sandbox build` creates.
const DefaultSandboxImage = "fleet-agent:latest"

// DefaultSandboxEnv lists the daemon environment variables passed into
// sandboxes when [sandbox] env is not set: credentials agents commonly need
// that cannot be read from the host's files inside a container. Claude
// Code's ANTHROPIC_API_KEY is deliberately missing (it switches to metered
// billing); add it to env to opt in.
var DefaultSandboxEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// SandboxConfig is the [sandbox] table.
type SandboxConfig struct {
	// Default is the sandbox of agents that do not ask for one: "none"
	// (default) or "docker".
	Default string `toml:"default,omitempty"`
	// Image is the Docker image. Default DefaultSandboxImage.
	Image string `toml:"image,omitempty"`
	// Network is passed to docker run --network. Default: Docker's bridge.
	Network string `toml:"network,omitempty"`
	// Env names daemon environment variables to pass into containers (when
	// set). nil means DefaultSandboxEnv.
	Env []string `toml:"env,omitempty"`
	// Args are extra docker run arguments, e.g. ["--memory=8g", "--cpus=4"].
	Args []string `toml:"args,omitempty"`
	// Docker is the docker CLI. Default "docker" from PATH.
	Docker string `toml:"docker,omitempty"`
	// Auth lists adapters whose host login sandboxed agents share, e.g.
	// ["claude", "codex"]: the daemon keeps their credentials file in sync
	// between the daemon user's home and the sandbox home.
	Auth []string `toml:"auth,omitempty"`
	// Dir holds everything containers mount from fleet: worktrees and
	// clones of sandboxed agents (worktrees/, clones/), their state dirs
	// (agents/<id>/), the sandbox HOME (home/) and a copy of the fleet
	// binary (bin/). Docker must be able to see it: a snap-packaged Docker
	// cannot see hidden folders such as ~/.fleet. A leading "~/" is
	// expanded. Default <FLEET_HOME>/sandbox.
	Dir string `toml:"dir,omitempty"`
}

// DirOrDefault returns Dir, with "~/" expanded, or <FLEET_HOME>/sandbox.
func (s SandboxConfig) DirOrDefault() string {
	switch {
	case s.Dir == "":
		return Path("sandbox")
	case strings.HasPrefix(s.Dir, "~/"):
		if u, err := os.UserHomeDir(); err == nil {
			return filepath.Join(u, s.Dir[2:])
		}
	}
	return filepath.Clean(s.Dir)
}

// ImageOrDefault returns Image or DefaultSandboxImage.
func (s SandboxConfig) ImageOrDefault() string {
	if s.Image == "" {
		return DefaultSandboxImage
	}
	return s.Image
}

// EnvOrDefault returns Env or DefaultSandboxEnv.
func (s SandboxConfig) EnvOrDefault() []string {
	if s.Env == nil {
		return DefaultSandboxEnv
	}
	return s.Env
}

// DockerOrDefault returns Docker or "docker".
func (s SandboxConfig) DockerOrDefault() string {
	if s.Docker == "" {
		return "docker"
	}
	return s.Docker
}

// Root is an allowed folder.
type Root struct {
	Name     string   `toml:"name"`
	Path     string   `toml:"path"`
	Adapters []string `toml:"adapters,omitempty"`
	// Trust pre-accepts the agent CLIs' folder trust prompt for agents
	// started here (see fleetv1.Root.trust).
	Trust bool `toml:"trust,omitempty"`
}

// AdapterConfig overrides adapter behaviour.
type AdapterConfig struct {
	Binary string   `toml:"binary"`
	Args   []string `toml:"args"`
}

// Proto converts a root to the wire type.
func (r Root) Proto() *fleetv1.Root {
	return &fleetv1.Root{Name: r.Name, Path: r.Path, Adapters: r.Adapters, Trust: r.Trust}
}

// ErrOutsideRoots is returned when a path is not inside any root.
var ErrOutsideRoots = errors.New("path is outside the configured roots")

// ErrUnknownRoot is returned for a root name that is not configured.
var ErrUnknownRoot = errors.New("no root named")

// Home returns FLEET_HOME or ~/.fleet (not created).
func Home() (string, error) {
	if h := os.Getenv("FLEET_HOME"); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(u, ".fleet"), nil
}

// EnsureHome creates the home dir (0700) and returns it.
func EnsureHome() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(h, 0o700); err != nil {
		return "", err
	}
	return h, nil
}

// Path joins elem onto Home. Panics only if Home fails; callers validated Home at startup.
func Path(elem ...string) string {
	h, err := Home()
	if err != nil {
		panic(err)
	}
	return filepath.Join(append([]string{h}, elem...)...)
}

// DefaultTmuxSocket is "fleet" for the default home (~/.fleet) and
// "fleet-<hash of FLEET_HOME>" otherwise, so a daemon on another home never
// shares (and reaps) the tmux server of the main daemon.
func DefaultTmuxSocket() string {
	h, err := Home()
	if err != nil {
		return "fleet"
	}
	if u, err := os.UserHomeDir(); err == nil && h == filepath.Join(u, ".fleet") {
		return "fleet"
	}
	sum := sha256.Sum256([]byte(h))
	return "fleet-" + hex.EncodeToString(sum[:4])
}

// SocketPath is Path("fleet.sock").
func SocketPath() string { return Path("fleet.sock") }

// Load reads config.toml, applying defaults. A missing file yields defaults.
func Load() (*Config, error) {
	h, err := Home()
	if err != nil {
		return nil, err
	}
	c := &Config{}
	md, err := toml.DecodeFile(filepath.Join(h, "config.toml"), c)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		md = toml.MetaData{}
	case err != nil:
		return nil, fmt.Errorf("config.toml: %w", err)
	}
	if !md.IsDefined("listen") {
		c.Listen = DefaultListen
	}
	if md.IsDefined("sandbox", "env") && c.Sandbox.Env == nil {
		c.Sandbox.Env = []string{} // env = [] passes nothing
	}
	if c.Sandbox.Dir != "" && !filepath.IsAbs(c.Sandbox.Dir) && !strings.HasPrefix(c.Sandbox.Dir, "~/") {
		return nil, fmt.Errorf("config.toml: [sandbox] dir must be absolute or start with ~/, not %q", c.Sandbox.Dir)
	}
	switch c.Sandbox.Default {
	case "", "none", "docker":
	default:
		return nil, fmt.Errorf("config.toml: [sandbox] default must be \"none\" or \"docker\", not %q", c.Sandbox.Default)
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Name == "" {
		if hn, err := os.Hostname(); err == nil {
			c.Name = hn
		} else {
			c.Name = "fleet"
		}
	}
	if c.TmuxSocket == "" {
		c.TmuxSocket = DefaultTmuxSocket()
	}
	if c.DefaultIsolation == "" {
		c.DefaultIsolation = "worktree"
	}
}

// Save writes config.toml atomically (temp file + rename, 0600).
func (c *Config) Save() error {
	h, err := EnsureHome()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return writeFileAtomic(filepath.Join(h, "config.toml"), buf.Bytes())
}

// MDNSEnabled reports the effective mdns setting.
func (c *Config) MDNSEnabled() bool { return c.MDNS == nil || *c.MDNS }

// writeFileAtomic writes data to a temp file next to path (0600), syncs it
// and renames it into place.
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
