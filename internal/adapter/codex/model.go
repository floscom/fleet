package codex

// The model and effort of a session (verified with Codex 0.154).
//
// Codex takes them as -m and -c model_reasoning_effort=... The models it
// offers, and the efforts each runs at, are in $CODEX_HOME/models_cache.json,
// which Codex fetches itself. Every turn starts with a turn_context line
// naming its model and effort. Codex's /model picker saves the pick to
// config.toml as the default for every later session, so a running session
// is not switched from the dashboard (no adapter.ModelSwitcher).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"fleet/internal/adapter"
)

var _ adapter.Modeler = (*codex)(nil)

// effortLabels names the reasoning efforts as Codex's picker does, in
// order, least first.
var effortLabels = []adapter.Choice{
	{ID: "none", Label: "None"}, {ID: "minimal", Label: "Minimal"}, {ID: "low", Label: "Low"},
	{ID: "medium", Label: "Medium"}, {ID: "high", Label: "High"}, {ID: "xhigh", Label: "Extra high"},
	{ID: "max", Label: "Max"}, {ID: "ultra", Label: "Ultra"},
}

// defaultEfforts are offered when Codex has no model list yet.
var defaultEfforts = []string{"low", "medium", "high", "xhigh"}

// modelCache keeps the models read from models_cache.json, again only when
// the file changed.
type modelCache struct {
	mu      sync.Mutex
	file    string
	size    int64
	modTime int64
	models  adapter.Models
}

// Models implements adapter.Modeler.
func (c *codex) Models() adapter.Models {
	file := ""
	if auth := c.AuthFile(""); auth != "" {
		file = filepath.Join(filepath.Dir(auth), "models_cache.json")
	}
	return c.models.get(file)
}

func (mc *modelCache) get(file string) adapter.Models {
	var size, mod int64
	if fi, err := os.Stat(file); file != "" && err == nil && fi.Mode().IsRegular() {
		size, mod = fi.Size(), fi.ModTime().UnixNano()
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.models.Efforts == nil || mc.file != file || mc.size != size || mc.modTime != mod {
		var data []byte
		if size > 0 && size < 8<<20 {
			data, _ = os.ReadFile(file)
		}
		mc.file, mc.size, mc.modTime, mc.models = file, size, mod, parseModels(data)
	}
	return mc.models
}

// parseModels reads models_cache.json: the models the picker lists, in its
// order, and the efforts they run at.
func parseModels(data []byte) adapter.Models {
	var f struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
			Priority    int    `json:"priority"`
			Levels      []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	_ = json.Unmarshal(data, &f)
	sort.SliceStable(f.Models, func(i, j int) bool { return f.Models[i].Priority < f.Models[j].Priority })
	var out adapter.Models
	used := map[string]bool{}
	for _, m := range f.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		mod := adapter.Model{ID: m.Slug, Label: m.Slug, Efforts: []string{}}
		for _, l := range m.Levels {
			if l.Effort != "" && !slices.Contains(mod.Efforts, l.Effort) {
				mod.Efforts = append(mod.Efforts, l.Effort)
				used[l.Effort] = true
			}
		}
		out.Models = append(out.Models, mod)
	}
	if len(out.Models) == 0 {
		for _, e := range defaultEfforts {
			used[e] = true
		}
	}
	out.Efforts = []adapter.Choice{}
	for _, e := range effortLabels {
		if used[e.ID] {
			out.Efforts = append(out.Efforts, e)
			delete(used, e.ID)
		}
	}
	// Levels newer than this list, by name.
	rest := make([]string, 0, len(used))
	for e := range used {
		rest = append(rest, e)
	}
	sort.Strings(rest)
	for _, e := range rest {
		out.Efforts = append(out.Efforts, adapter.Choice{ID: e, Label: e})
	}
	return out
}

var turnContextMark = []byte(`"turn_context"`)

// ModelLine implements adapter.Modeler: turn_context lines name the model
// and effort of their turn.
func (c *codex) ModelLine(line []byte, m *adapter.SessionModel) {
	if !bytes.Contains(line, turnContextMark) {
		return
	}
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"payload"`
	}
	if !decode(line, &l) || l.Type != "turn_context" || l.Payload.Model == "" {
		return
	}
	m.Name, m.Model, m.Effort = l.Payload.Model, "", l.Payload.Effort
	if _, ok := c.Models().Find(l.Payload.Model); ok {
		m.Model = l.Payload.Model
	}
}
