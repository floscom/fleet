// Package config owns fleet's on-disk layout and config.toml.
//
// Layout (FLEET_HOME, default ~/.fleet, mode 0700):
//
//	config.toml        user config (roots, listen address, name)
//	fleet.sock         local control socket (mode 0600)
//	daemon.pid         pid of the running daemon
//	daemon.log         daemon log when started via `fleet start`
//	identity/          server TLS key+cert, client device key (0600)
//	devices.json       paired devices (0600)
//	servers.json       daemons this machine has paired with as a client (0600)
//	agents.json        agent registry (0600)
//	agents/<id>/       per-agent state dir (adapter files)
//	worktrees/         git worktrees created for agents
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

	fleetv1 "fleet/gen/fleetv1"

	"github.com/BurntSushi/toml"
)

// DefaultListen is the default TCP listen address for remote clients.
const DefaultListen = "0.0.0.0:7420"

// Config is config.toml.
type Config struct {
	// Name shown to clients and in mDNS. Default: hostname.
	Name string `toml:"name"`
	// Listen is the TCP address for remote (TLS) clients. "" or "off" disables it.
	Listen string `toml:"listen"`
	// MDNS enables LAN advertisement. Default true.
	MDNS *bool `toml:"mdns"`
	// TmuxSocket is the tmux -L socket name. Default: DefaultTmuxSocket.
	TmuxSocket string `toml:"tmux_socket"`
	// DefaultIsolation: "worktree" (default; falls back to pinned outside git) or "pinned".
	DefaultIsolation string `toml:"default_isolation"`
	Roots            []Root `toml:"root"`
	// Adapter-specific overrides, e.g. [adapter.claude] binary = "/opt/claude".
	Adapters map[string]AdapterConfig `toml:"adapter"`
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
