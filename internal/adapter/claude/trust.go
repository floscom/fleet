package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Claude Code keeps folder trust in its global config file (~/.claude.json)
// as projects["<dir>"].hasTrustDialogAccepted. Answering "Yes, I trust this
// folder" sets it for the git repository root (the main checkout for a
// worktree) or, outside git, the directory itself. A trusted directory also
// covers the directories below it, but only down to the enclosing git root:
// trusting ~/code does not cover the repository ~/code/api. Verified with
// Claude Code 2.1.280.
//
// Every running Claude instance rewrites that file, always read-modify-write
// under the lock directory "<file>.lock" (proper-lockfile: created with
// mkdir, taken over when its mtime is stale). trustFolder takes the same
// lock, so neither side loses the other's change.

const (
	// lockStale matches proper-lockfile's default: holders refresh the lock's
	// mtime every 5s, so one older than 10s belongs to a dead process.
	lockStale = 10 * time.Second
	// lockWait is long enough to outwait a stale lock left by a crash.
	lockWait = lockStale + 3*time.Second
)

// projectDefaults is the entry Claude creates for a new project (2.1.280).
var projectDefaults = map[string]json.RawMessage{
	"allowedTools":                            json.RawMessage(`[]`),
	"mcpContextUris":                          json.RawMessage(`[]`),
	"mcpServers":                              json.RawMessage(`{}`),
	"enabledMcpjsonServers":                   json.RawMessage(`[]`),
	"disabledMcpjsonServers":                  json.RawMessage(`[]`),
	"hasTrustDialogAccepted":                  json.RawMessage(`false`),
	"hasClaudeMdExternalIncludesApproved":     json.RawMessage(`false`),
	"hasClaudeMdExternalIncludesWarningShown": json.RawMessage(`false`),
}

// globalConfigFile is where Claude Code keeps its global config: a legacy
// .config.json in the config dir if one exists, else .claude.json in
// $CLAUDE_CONFIG_DIR or the home directory.
func globalConfigFile() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	home, err := os.UserHomeDir()
	if err != nil && dir == "" {
		return "", err
	}
	configDir := dir
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	if legacy := filepath.Join(configDir, ".config.json"); fileExists(legacy) {
		return legacy, nil
	}
	if dir == "" {
		dir = home
	}
	return filepath.Join(dir, ".claude.json"), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// seedConfig creates Claude's global config file with onboarding marked as
// done, unless it exists. Used for the private home of sandboxed agents,
// where the theme and login walkthrough would greet every new container.
// Logging in (CLAUDE_CODE_OAUTH_TOKEN, or /login once) is still needed.
func seedConfig(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("{\n  \"hasCompletedOnboarding\": true\n}\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// trustFolder marks dir as trusted in Claude's global config file. A missing
// file is left alone: Claude has not been set up yet and will show its
// onboarding (and the trust prompt) anyway.
func trustFolder(ctx context.Context, file, dir string) error {
	if !fileExists(file) {
		return nil
	}
	unlock, err := lockFile(ctx, file+".lock")
	if err != nil {
		return err
	}
	defer unlock()

	fi, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	out, changed, err := setTrusted(data, dir)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if !changed {
		return nil
	}
	// Replace the target of a symlinked config (dotfile managers), not the link.
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	return writeFileAtomic(file, out, fi.Mode().Perm())
}

// setTrusted returns config with projects[dir].hasTrustDialogAccepted set.
// Values it does not touch are kept byte for byte (keys end up sorted).
func setTrusted(config []byte, dir string) (out []byte, changed bool, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(config, &top); err != nil {
		return nil, false, err
	}
	if top == nil {
		return nil, false, errors.New("not a JSON object")
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := top["projects"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return nil, false, fmt.Errorf("projects: %w", err)
		}
	}
	p := projects[dir]
	if p != nil && string(p["hasTrustDialogAccepted"]) == "true" {
		return config, false, nil
	}
	if p == nil {
		p = make(map[string]json.RawMessage, len(projectDefaults))
		for k, v := range projectDefaults {
			p[k] = v
		}
		projects[dir] = p
	}
	p["hasTrustDialogAccepted"] = json.RawMessage(`true`)
	if top["projects"], err = json.Marshal(projects); err != nil {
		return nil, false, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(top); err != nil {
		return nil, false, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), true, nil
}

// lockFile takes a proper-lockfile style lock: the directory path exists
// while the lock is held. A lock whose mtime is older than lockStale is
// taken over.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	delay := 20 * time.Millisecond
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > lockStale {
			// Stale: remove and retry at once. Another waiter may win the race;
			// then Mkdir fails again and this loop keeps waiting.
			os.Remove(path)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s is held by another process", path)
		case <-time.After(delay):
		}
		delay = min(2*delay, 500*time.Millisecond)
	}
}

// writeFileAtomic replaces path via a temporary file in the same directory.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fleet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
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
	return os.Rename(f.Name(), path)
}
