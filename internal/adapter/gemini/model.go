package gemini

import (
	"bytes"
	"encoding/json"

	"fleet/internal/adapter"
)

// Gemini takes the model as --model: an alias (auto, the default, lets
// Gemini pick per request; pro, flash, flash-lite) or a model id. It has no
// reasoning effort setting. Each answer in the transcript names the model
// that wrote it. Its /model picker switches the running session, but by a
// menu fleet does not drive (no adapter.ModelSwitcher). Verified with
// 0.66.0, whose manual list depends on the account.

var _ adapter.Modeler = (*gemini)(nil)

var models = adapter.Models{
	Models: []adapter.Model{
		{ID: "auto", Label: "Auto"},
		{ID: "pro", Label: "Pro"},
		{ID: "flash", Label: "Flash"},
		{ID: "flash-lite", Label: "Flash Lite"},
		{ID: "gemini-3.1-pro-preview", Label: "gemini-3.1-pro-preview"},
		{ID: "gemini-3.8-flash", Label: "gemini-3.8-flash"},
		{ID: "gemini-3-flash-preview", Label: "gemini-3-flash-preview"},
		{ID: "gemini-3.5-flash-lite", Label: "gemini-3.5-flash-lite"},
		{ID: "gemini-2.5-pro", Label: "gemini-2.5-pro"},
	},
	Efforts: []adapter.Choice{},
}

// Models implements adapter.Modeler.
func (g *gemini) Models() adapter.Models { return models }

var modelMark = []byte(`"model"`)

// ModelLine implements adapter.Modeler: answers name their model.
func (g *gemini) ModelLine(line []byte, m *adapter.SessionModel) {
	if !bytes.Contains(line, modelMark) {
		return
	}
	var r struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "gemini" || r.Model == "" {
		return
	}
	m.Name, m.Model = r.Model, ""
	if _, ok := models.Find(r.Model); ok {
		m.Model = r.Model
	}
}
