// Package detect locates agent CLI binaries and probes their version. It is
// shared by the built-in adapters so each one only states its binary name.
package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"fleet/internal/adapter"
)

// VersionTimeout bounds a single `<bin> --version` probe.
const VersionTimeout = 3 * time.Second

// WellKnownDirs are searched after PATH, because agent CLIs are commonly
// installed in per-user directories that are missing from the PATH of
// non-login shells and service managers. "~" is expanded.
var WellKnownDirs = []string{
	"~/.local/bin",
	"~/.claude/local",
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"~/.npm-global/bin",
}

// Binary finds one executable and caches its version. The zero value is not
// usable; set Name. Binary is safe for concurrent use.
type Binary struct {
	// Name is the executable name, e.g. "claude".
	Name string
	// Override is a user-configured name or path that replaces the search.
	Override string
	// Dirs replaces WellKnownDirs when non-nil (tests).
	Dirs []string
	// NoVersion skips the version probe (e.g. for shells).
	NoVersion bool

	mu    sync.Mutex
	cache map[cacheKey]probe
}

type cacheKey struct {
	path  string
	size  int64
	mtime time.Time
}

type probe struct {
	version string
	err     error
}

// Find returns the absolute path of the executable.
func (b *Binary) Find() (string, error) {
	if b.Override != "" {
		return findOverride(b.Override)
	}
	if p, err := exec.LookPath(b.Name); err == nil {
		return filepath.Abs(p)
	}
	dirs := b.Dirs
	if dirs == nil {
		dirs = WellKnownDirs
	}
	for _, d := range dirs {
		p := filepath.Join(expandHome(d), b.Name)
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH or %s", b.Name, strings.Join(dirs, ", "))
}

// Detect finds the binary and probes `<bin> --version`. Probes are cached per
// path, size and modification time, so repeated calls are cheap and an
// upgraded binary is re-probed.
func (b *Binary) Detect(ctx context.Context) adapter.Detection {
	path, err := b.Find()
	if err != nil {
		return adapter.Detection{Reason: err.Error()}
	}
	if b.NoVersion {
		return adapter.Detection{Available: true, Path: path}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return adapter.Detection{Path: path, Reason: err.Error()}
	}
	key := cacheKey{path, fi.Size(), fi.ModTime()}

	b.mu.Lock()
	p, ok := b.cache[key]
	b.mu.Unlock()
	if !ok {
		p.version, p.err = Version(ctx, path)
		// Timeouts and cancellations are transient; do not cache them.
		if p.err == nil || !errors.Is(p.err, context.DeadlineExceeded) && !errors.Is(p.err, context.Canceled) {
			b.mu.Lock()
			if b.cache == nil {
				b.cache = map[cacheKey]probe{}
			}
			b.cache[key] = p
			b.mu.Unlock()
		}
	}
	if p.err != nil {
		return adapter.Detection{Path: path, Reason: p.err.Error()}
	}
	return adapter.Detection{Available: true, Path: path, Version: p.version}
}

var versionRE = regexp.MustCompile(`\d+(\.\d+)+`)

// Version runs `<path> --version` with VersionTimeout and extracts the first
// dotted version number ("2.1.280 (Claude Code)" -> "2.1.280"). If there is
// none, the first output line is returned.
func Version(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, VersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s --version: %w", path, ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	s := strings.TrimSpace(string(out))
	if v := versionRE.FindString(s); v != "" {
		return v, nil
	}
	first, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(first), nil
}

func findOverride(o string) (string, error) {
	o = expandHome(o)
	if !strings.ContainsRune(o, filepath.Separator) {
		p, err := exec.LookPath(o)
		if err != nil {
			return "", err
		}
		return filepath.Abs(p)
	}
	p, err := filepath.Abs(o)
	if err != nil {
		return "", err
	}
	if !isExecutable(p) {
		return "", fmt.Errorf("%s is not an executable file", p)
	}
	return p, nil
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
