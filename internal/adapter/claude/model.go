package claude

// The model and effort of a session (verified with Claude Code 2.1.280).
//
// Claude Code takes them as --model and --effort. In a running session,
// /model opens a picker: ↑/↓ choose a model, ←/→ its effort, and "s" applies
// both to this session only. Enter, or a model or level typed after /model
// or /effort, would also save them as the user's default for new sessions,
// so SwitchModel drives the picker. With a conversation cached for the
// current model, Claude then asks "Switch model?" first.
//
// Every reply in the transcript names its model (message.model) and effort.
// A switch made in the picker is recorded as the command's output ("Set
// model to `Fable 5.1` for this session only with `high` effort"), except
// one confirmed in "Switch model?", which shows only on the screen.
// Ultracode (xhigh effort plus workflows) is recorded as xhigh.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"fleet/internal/adapter"
)

var (
	_ adapter.Modeler       = (*claude)(nil)
	_ adapter.ModelSwitcher = (*claude)(nil)
)

// allEfforts are the IDs of every effort level.
var allEfforts = []string{"low", "medium", "high", "xhigh", "max", "ultracode"}

// models are the rows of the /model picker, which also takes them as
// --model aliases. Its first row, "Default", is Opus.
var models = adapter.Models{
	Models: []adapter.Model{
		{ID: "opus", Label: "Opus", Efforts: allEfforts},
		{ID: "fable", Label: "Fable", Efforts: allEfforts},
		{ID: "sonnet", Label: "Sonnet", Efforts: allEfforts},
		{ID: "haiku", Label: "Haiku"},
	},
	Efforts: []adapter.Choice{
		{ID: "low", Label: "Low"}, {ID: "medium", Label: "Medium"}, {ID: "high", Label: "High"},
		{ID: "xhigh", Label: "xHigh"}, {ID: "max", Label: "Max"}, {ID: "ultracode", Label: "Ultracode"},
	},
}

// Models implements adapter.Modeler.
func (c *claude) Models() adapter.Models { return models }

// modelID is the ID of the model a name ("claude-opus-5-5", "Fable 5.1")
// belongs to, "" if none.
func modelID(name string) string {
	name = strings.ToLower(name)
	for _, m := range models.Models {
		if strings.Contains(name, m.ID) {
			return m.ID
		}
	}
	return ""
}

// effortID is the ID of an effort level as Claude writes it ("xHigh"), ""
// if there is none such.
func effortID(s string) string {
	s = strings.ToLower(s)
	if slices.Contains(allEfforts, s) {
		return s
	}
	return ""
}

var (
	// Lines of replies hold assistantMark (see workflow.go).
	setModelMark  = []byte("Set model to ")
	setEffortMark = []byte("Set effort level to ")

	// setModelRe matches a /model output, with the model's name in
	// backticks (newer versions) or not.
	setModelRe = regexp.MustCompile("^Set model to (?:`([^`]+)`|(.+?))(?: for this session only| and saved as your default.*)?(?: with `?[A-Za-z]+`? effort)?$")
	// withEffortRe finds the effort a /model output names.
	withEffortRe  = regexp.MustCompile(" with `?([A-Za-z]+)`? effort")
	setEffortRe   = regexp.MustCompile("^Set effort level to `?([A-Za-z]+)")
	commandOutTag = "local-command-stdout"
)

// ModelLine implements adapter.Modeler.
func (c *claude) ModelLine(line []byte, m *adapter.SessionModel) {
	switch {
	case bytes.Contains(line, assistantMark):
		var l struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Effort      string `json:"effort"`
			Message     struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		// API errors are written as replies of model "<synthetic>".
		if !decode(line, &l) || l.Type != "assistant" || l.IsSidechain || l.Message.Model == "" || strings.HasPrefix(l.Message.Model, "<") {
			return
		}
		effort := effortID(l.Effort)
		if effort == "xhigh" && m.Effort == "ultracode" {
			effort = "ultracode"
		}
		setModel(m, modelID(l.Message.Model), l.Message.Model, effort)
	case bytes.Contains(line, setModelMark) || bytes.Contains(line, setEffortMark):
		var l struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if !decode(line, &l) {
			return
		}
		raw := l.Message.Content
		if l.Type == "system" {
			raw = l.Content
		} else if l.Type != "user" {
			return
		}
		for _, s := range stringsOf(raw) {
			out, ok := inner(s, commandOutTag)
			if !ok {
				continue
			}
			out, _, _ = strings.Cut(strings.TrimSpace(out), "\n")
			if sm := setModelRe.FindStringSubmatch(out); sm != nil {
				name := sm[1] + sm[2]
				effort := ""
				if e := withEffortRe.FindStringSubmatch(out); e != nil {
					effort = effortID(e[1])
				}
				setModel(m, modelID(name), name, effort)
			} else if se := setEffortRe.FindStringSubmatch(out); se != nil {
				if e := effortID(se[1]); e != "" {
					m.Effort = e
				}
			}
		}
	}
}

// setModel records a model and, if known, its effort. A model without
// effort levels has none.
func setModel(m *adapter.SessionModel, id, name, effort string) {
	m.Model, m.Name = id, name
	if mod, ok := models.Find(id); ok && len(mod.Efforts) == 0 {
		m.Effort = ""
	} else if effort != "" {
		m.Effort = effort
	}
}

// Picker timing: how long to wait for the screen to show a key's effect
// (tests shorten it), and how often to look.
var pickerWait = 3 * time.Second

const pickerPoll = 60 * time.Millisecond

// SwitchModel implements adapter.ModelSwitcher: it opens /model, moves to
// the model and the effort, and presses "s" (this session only).
func (c *claude) SwitchModel(ctx context.Context, term adapter.Terminal, model, effort string) (err error) {
	if model == "" && effort == "" {
		return nil
	}
	label := model
	if model != "" {
		m, ok := models.Find(model)
		if !ok {
			return fmt.Errorf("unknown model %q", model)
		}
		label = m.Label
	}
	if effort != "" && !models.HasEffort(effort) {
		return fmt.Errorf("unknown effort %q", effort)
	}
	screen, err := term.Screen(ctx)
	if err != nil {
		return err
	}
	if c.DialogOpen(screen) {
		return errors.New("Claude shows a dialog: answer it first")
	}
	if err := term.Type(ctx, "/model", true); err != nil {
		return err
	}
	var p picker
	if _, err := await(ctx, term, func(s string) bool {
		var ok bool
		p, ok = parsePicker(s)
		return ok
	}); err != nil {
		return fmt.Errorf("the model picker did not open: %w", err)
	}
	defer func() {
		// Leave no picker open, even when ctx ended.
		if err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if s, e := term.Screen(ctx); e == nil {
			if _, open := parsePicker(s); open {
				_ = term.Press(ctx, "Escape")
			}
		}
	}()

	if model != "" {
		want := p.row(model)
		if want < 0 || p.cursor < 0 {
			return fmt.Errorf("the model picker offers no %s", label)
		}
		if d := want - p.cursor; d != 0 {
			key := "Down"
			if d < 0 {
				key, d = "Up", -d
			}
			keys := make([]string, d)
			for i := range keys {
				keys[i] = key
			}
			if err := term.Press(ctx, keys...); err != nil {
				return err
			}
			if _, err := await(ctx, term, func(s string) bool {
				p, _ = parsePicker(s)
				return p.cursor == want
			}); err != nil {
				return fmt.Errorf("could not select %s: %w", label, err)
			}
		}
	}

	// A model without effort levels shows none; the effort is then moot.
	if effort != "" && p.effort != "" && p.effort != effort {
		want := slices.Index(allEfforts, effort)
		key := "Right"
		if want < slices.Index(allEfforts, p.effort) {
			key = "Left"
		}
		// One step at a time: a model may skip levels.
		for range allEfforts {
			was := p.effort
			if err := term.Press(ctx, key); err != nil {
				return err
			}
			if _, err := await(ctx, term, func(s string) bool {
				p, _ = parsePicker(s)
				return p.effort != was
			}); err != nil {
				return fmt.Errorf("could not set the effort: %w", err)
			}
			if p.effort == effort {
				break
			}
		}
		if p.effort != effort {
			return fmt.Errorf("%s does not run at %s effort", label, effort)
		}
	}

	if err := term.Press(ctx, "s"); err != nil {
		return err
	}
	screen, err = await(ctx, term, func(s string) bool {
		_, open := parsePicker(s)
		return !open
	})
	if err != nil {
		return fmt.Errorf("the model picker did not close: %w", err)
	}
	// "Switch model?" replaces the picker when the conversation is cached
	// for the old model; the user asked to switch.
	yes, ok := parseConfirm(screen)
	if !ok {
		time.Sleep(3 * pickerPoll)
		if screen, err = term.Screen(ctx); err != nil {
			return err
		}
		yes, ok = parseConfirm(screen)
	}
	if !ok {
		return nil
	}
	if !yes {
		return errors.New("Claude asks whether to switch: answer it on the screen")
	}
	if err := term.Press(ctx, "Enter"); err != nil {
		return err
	}
	if _, err := await(ctx, term, func(s string) bool {
		_, open := parseConfirm(s)
		return !open
	}); err != nil {
		return fmt.Errorf("could not confirm the switch: %w", err)
	}
	return nil
}

// await looks at the screen until cond holds, for up to pickerWait.
func await(ctx context.Context, term adapter.Terminal, cond func(screen string) bool) (string, error) {
	deadline := time.Now().Add(pickerWait)
	for {
		screen, err := term.Screen(ctx)
		if err != nil {
			return "", err
		}
		if cond(screen) {
			return screen, nil
		}
		if time.Now().After(deadline) {
			return screen, errors.New("timed out")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pickerPoll):
		}
	}
}

// picker is what the screen shows of the /model picker.
type picker struct {
	// rows are the models' labels ("Opus (1M context)"), cursor the index
	// of the highlighted one (-1 if none).
	rows   []string
	cursor int
	// effort is the ID of the effort shown for it, "" if it has none.
	effort string
}

// row returns the index of the row of model id, -1 if none.
func (p picker) row(id string) int {
	for i, r := range p.rows {
		if first, _, _ := strings.Cut(strings.ToLower(r), " "); first == id {
			return i
		}
	}
	return -1
}

const (
	// pickerHint is one of the hints in the /model picker's footer.
	pickerHint = "s to use this session only"
	// pickerTitle heads it.
	pickerTitle = "Select model"
	// confirmTitle heads the dialog that asks whether to switch.
	confirmTitle = "Switch model?"
)

var (
	// rowRe matches a row of a select: "❯ 2. Opus (1M context)  Opus 5.5 …".
	rowRe = regexp.MustCompile(`^(❯\s+)?(\d+)\.\s+(.*)$`)
	// pickerEffortRe matches the effort line: "◐ Medium effort (default) ←/→ to adjust".
	pickerEffortRe = regexp.MustCompile(`^\S+\s+([A-Za-z]+) effort\b.*to adjust`)
)

// parsePicker reads the /model picker off the screen, if it is open: its
// footer at the bottom, its title above.
func parsePicker(screen string) (picker, bool) {
	lines := strings.Split(screen, "\n")
	foot := footer(lines, pickerHint)
	if foot < 0 {
		return picker{}, false
	}
	top := -1
	for i := foot - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == pickerTitle {
			top = i
			break
		}
	}
	if top < 0 {
		return picker{}, false
	}
	p := picker{cursor: -1}
	for _, l := range lines[top+1 : foot] {
		l = strings.TrimSpace(l)
		if m := rowRe.FindStringSubmatch(l); m != nil {
			label, _, _ := strings.Cut(m[3], "  ")
			if m[1] != "" {
				p.cursor = len(p.rows)
			}
			p.rows = append(p.rows, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(label), "✔")))
		} else if m := pickerEffortRe.FindStringSubmatch(l); m != nil {
			p.effort = effortID(m[1])
		}
	}
	return p, len(p.rows) > 0
}

// parseConfirm reads the "Switch model?" dialog off the screen, if it is
// open, and reports whether its first option ("Yes, switch to …") is
// highlighted. The dialog has no footer: its title is near the bottom,
// with two lines of text and its options below.
func parseConfirm(screen string) (yes, ok bool) {
	lines := strings.Split(screen, "\n")
	for i, n := len(lines)-1, 0; i >= 0 && n < footerLines+2; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		n++
		if l != confirmTitle {
			continue
		}
		for _, r := range lines[i+1:] {
			m := rowRe.FindStringSubmatch(strings.TrimSpace(r))
			if m != nil && m[2] == "1" && strings.HasPrefix(m[3], "Yes, switch") {
				return m[1] != "", true
			}
		}
		return false, false
	}
	return false, false
}

// footer returns the index of the line near the bottom of the screen whose
// " · "-separated hints include hint, -1 if none.
func footer(lines []string, hint string) int {
	for i, n := len(lines)-1, 0; i >= 0 && n < footerLines; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if slices.Contains(strings.Split(l, " · "), hint) {
			return i
		}
		n++
	}
	return -1
}
