package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Gemini loads every directory in <gemini dir>/extensions that holds a
// gemini-extension.json, with no install step, and runs the hooks in its
// hooks/hooks.json (same shape as the "hooks" of settings.json) in trusted
// and untrusted folders alike (0.66.0). A hook prints nothing and exits 0
// to leave Gemini alone: text on stdout or stderr is shown in its terminal.

// extensionName is the directory and name of fleet's extension.
const extensionName = "fleet"

// hookEvents are the Gemini hook events fleet subscribes to (see
// HandleHook).
var hookEvents = []string{"SessionStart", "BeforeAgent", "AfterTool", "Notification", "AfterAgent"}

// hookTimeout bounds a hook run, in milliseconds.
const hookTimeout = 10000

// hookCommand is the command of fleet's hook for event: it reports the
// payload with `fleet hook` in sessions fleet started (envAgent set) and
// reads and drops it in every other.
func hookCommand(event string) string {
	return fmt.Sprintf(`if [ -n "$%s" ]; then "$%s" hook --agent "$%s" --adapter %s %s; else cat; fi >/dev/null 2>&1; exit 0`,
		envAgent, envHook, envAgent, ID, event)
}

// extensionFiles are the files of fleet's extension, by path inside it.
func extensionFiles() map[string][]byte {
	type handler struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Hooks []handler `json:"hooks"`
	}
	hooks := map[string][]group{}
	for _, ev := range hookEvents {
		hooks[ev] = []group{{Hooks: []handler{{Name: "fleet", Type: "command", Command: hookCommand(ev), Timeout: hookTimeout}}}}
	}
	return map[string][]byte{
		"gemini-extension.json": marshal(map[string]any{
			"name":        extensionName,
			"version":     "1.0.0",
			"description": "Reports the state of Gemini sessions fleet runs to the fleet daemon; does nothing in other sessions.",
		}),
		filepath.Join("hooks", "hooks.json"): marshal(map[string]any{"hooks": hooks}),
	}
}

func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// installExtension writes fleet's extension into dir (a Gemini dir), where
// it differs from what is there.
func installExtension(dir string) error {
	if dir == "" {
		return errors.New("no home directory for Gemini's extension")
	}
	root := filepath.Join(dir, "extensions", extensionName)
	for name, data := range extensionFiles() {
		path := filepath.Join(root, name)
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			continue
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(path, data); err != nil {
			return fmt.Errorf("installing the extension: %w", err)
		}
	}
	return nil
}

// writeFileAtomic replaces path via a temporary file in the same directory,
// so a Gemini starting meanwhile never reads half a file.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+strings.TrimPrefix(filepath.Base(path), ".")+".fleet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
