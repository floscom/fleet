package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"fleet/internal/adapter"
)

// modelsCache is models_cache.json as Codex 0.154 writes it, cut down.
const modelsCache = `{"client_version":"0.154.0","etag":"x","fetched_at":"2026-09-30T08:00:00Z","models":[
{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":12,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}]},
{"slug":"gpt-reserve","display_name":"GPT-Reserve","visibility":"hide","priority":3,"supported_reasoning_levels":[{"effort":"low"},{"effort":"hyper"}]},
{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","visibility":"list","priority":1,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"},{"effort":"turbo"}]}
]}`

func TestModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	c := New(fakeCodex(t), nil).(*codex)

	// Without Codex's list: no models, the common efforts.
	ms := c.Models()
	if len(ms.Models) != 0 || len(ms.Efforts) != 4 || ms.Efforts[3] != (adapter.Choice{ID: "xhigh", Label: "Extra high"}) {
		t.Fatalf("no cache: %+v", ms)
	}

	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(modelsCache), 0o600); err != nil {
		t.Fatal(err)
	}
	ms = c.Models()
	want := adapter.Models{
		Models: []adapter.Model{
			{ID: "gpt-6-astra", Label: "gpt-6-astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra", "turbo"}},
			{ID: "gpt-5.5", Label: "gpt-5.5", Efforts: []string{"low", "medium", "high", "xhigh"}},
		},
		Efforts: []adapter.Choice{
			{ID: "low", Label: "Low"}, {ID: "medium", Label: "Medium"}, {ID: "high", Label: "High"},
			{ID: "xhigh", Label: "Extra high"}, {ID: "max", Label: "Max"}, {ID: "ultra", Label: "Ultra"},
			{ID: "turbo", Label: "turbo"},
		},
	}
	if !reflect.DeepEqual(ms, want) {
		t.Fatalf("models\n%+v\nwant\n%+v", ms, want)
	}
}

func TestModelLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(modelsCache), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(fakeCodex(t), nil).(*codex)
	var m adapter.SessionModel
	c.ModelLine([]byte(`{"timestamp":"2026-09-28T21:55:00.824Z","type":"turn_context","payload":{"cwd":"/w","model":"gpt-6-astra","effort":"max","summary":"auto"}}`), &m)
	if m != (adapter.SessionModel{Model: "gpt-6-astra", Name: "gpt-6-astra", Effort: "max"}) {
		t.Fatalf("turn_context: %+v", m)
	}
	c.ModelLine([]byte(`{"type":"event_msg","payload":{"type":"item_completed","item":{"text":"\"turn_context\" is a line type"}}}`), &m)
	c.ModelLine([]byte(`{"type":"turn_context","payload":{"model":"gpt-reserve","effort":"low"}}`), &m)
	if m != (adapter.SessionModel{Name: "gpt-reserve", Effort: "low"}) {
		t.Fatalf("unlisted model: %+v", m)
	}
}

func TestLaunchModel(t *testing.T) {
	spec, err := New(fakeCodex(t), nil).Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "a", FleetBinary: "/f", Prompt: "go", Model: "gpt-5.5", Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tail := spec.Argv[len(spec.Argv)-6:]; !slices.Equal(tail, []string{"-m", "gpt-5.5", "-c", `model_reasoning_effort="high"`, "--", "go"}) {
		t.Fatalf("argv ends in %q", tail)
	}
}
