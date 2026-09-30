package claude

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/internal/adapter"
)

func TestModelLine(t *testing.T) {
	c := &claude{}
	cmd := func(typ, out string) string {
		content := fmt.Sprintf("%q", "<local-command-stdout>"+out+"</local-command-stdout>")
		if typ == "system" {
			return `{"type":"system","subtype":"local_command","content":` + content + `}`
		}
		return `{"type":"user","message":{"role":"user","content":` + content + `}}`
	}
	reply := func(model, effort string) string {
		return `{"type":"assistant","effort":"` + effort + `","message":{"model":"` + model + `","content":[{"type":"text","text":"ok"}]}}`
	}
	steps := []struct {
		line string
		want adapter.SessionModel
	}{
		{reply("claude-opus-5-5", "xhigh"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "xhigh"}},
		// Not the session's own model: subagents, API errors.
		{`{"type":"assistant","isSidechain":true,"effort":"low","message":{"model":"claude-haiku-4-5"}}`, adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "xhigh"}},
		{reply("<synthetic>", ""), adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "xhigh"}},
		{cmd("user", "Set model to `Fable 5.1` for this session only with `high` effort"), adapter.SessionModel{Model: "fable", Name: "Fable 5.1", Effort: "high"}},
		{cmd("system", "Kept model as `Fable 5.1`"), adapter.SessionModel{Model: "fable", Name: "Fable 5.1", Effort: "high"}},
		{cmd("user", "Set model to `Sonnet 5` and saved as your default for new sessions"), adapter.SessionModel{Model: "sonnet", Name: "Sonnet 5", Effort: "high"}},
		{cmd("system", "Set model to claude-opus-5"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5", Effort: "high"}},
		// Ultracode is written as xhigh.
		{cmd("user", "Set effort level to ultracode (this session only): xhigh + dynamic workflow orchestration"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5", Effort: "ultracode"}},
		{reply("claude-opus-5-5", "xhigh"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "ultracode"}},
		{cmd("user", "Set effort level to max (this session only): Maximum capability"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "max"}},
		{reply("claude-opus-5-5", "xhigh"), adapter.SessionModel{Model: "opus", Name: "claude-opus-5-5", Effort: "xhigh"}},
		// Haiku has no effort levels.
		{reply("claude-haiku-4-5-20251001", ""), adapter.SessionModel{Model: "haiku", Name: "claude-haiku-4-5-20251001"}},
		{reply("claude-future-9", "low"), adapter.SessionModel{Name: "claude-future-9", Effort: "low"}},
		// Quoted, not run: a message about the command.
		{`{"type":"user","message":{"role":"user","content":"Set model to opus, please"}}`, adapter.SessionModel{Name: "claude-future-9", Effort: "low"}},
	}
	var m adapter.SessionModel
	for i, s := range steps {
		c.ModelLine([]byte(s.line), &m)
		if m != s.want {
			t.Fatalf("step %d: %+v, want %+v", i, m, s.want)
		}
	}
}

func TestLaunchModel(t *testing.T) {
	bin := fakeClaude(t)
	a := New(bin, nil)
	spec, err := a.Launch(context.Background(), adapter.LaunchRequest{
		AgentID: "a1", Cwd: "/work", Prompt: "go", Model: "fable", Effort: "ultracode",
		FleetBinary: "/usr/bin/fleet", StateDir: filepath.Join(t.TempDir(), "a1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tail := spec.Argv[len(spec.Argv)-6:]; !slices.Equal(tail, []string{"--model", "fable", "--effort", "ultracode", "--", "go"}) {
		t.Fatalf("argv ends in %q", tail)
	}
}

// pickerScreen is the /model picker as Claude Code 2.1.280 draws it.
const pickerScreen = ` ▐▛███▛█   Claude Code v2.1.280
▝▜██████▀  Opus 5.5 (1M context) · Claude Max
  ▝▝ ▝▝    /tmp/mfx

❯ /model
▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔ ◐ medium · /effort ▔
   Select model
   Switch between Claude models. Your pick becomes the default for new sessions. For other/previous model names, specify with --model.

     1. Default (recommended)  Opus 5.5 with 1M context · Best for everyday, complex tasks
     2. Opus (1M context)      Opus 5.5 with 1M context · Best for everyday, complex tasks
   ❯ 3. Fable ✔                Fable 5.1 · Most capable for your hardest and longest-running tasks
     4. Sonnet                 Sonnet 5 · Efficient for routine tasks
     5. Haiku                  Haiku 4.5 · Fastest for quick answers

   ◉ xHigh effort (default) ←/→ to adjust

   Use /fast to turn on Fast mode (Opus 5.5).

   Enter to set as default · s to use this session only · Esc to cancel

`

func TestParsePicker(t *testing.T) {
	p, ok := parsePicker(pickerScreen)
	if !ok || p.cursor != 2 || p.effort != "xhigh" || !slices.Equal(p.rows, []string{"Default (recommended)", "Opus (1M context)", "Fable", "Sonnet", "Haiku"}) {
		t.Fatalf("picker %+v, %v", p, ok)
	}
	if p.row("sonnet") != 3 || p.row("opus") != 1 || p.row("gpt") != -1 {
		t.Fatal("row lookup")
	}
	haiku := strings.Replace(pickerScreen, "   ◉ xHigh effort (default) ←/→ to adjust", "   ○ Effort not supported for Haiku", 1)
	if p, ok := parsePicker(haiku); !ok || p.effort != "" {
		t.Fatalf("haiku picker %+v", p)
	}
	// The picker's words further up, in the conversation, are no picker.
	quoted := pickerScreen + "● done\n\n────\n❯ \n────\n  ⏵⏵ auto mode on · a · b · c\n"
	if _, ok := parsePicker(quoted + strings.Repeat("line\n", footerLines)); ok {
		t.Fatal("a picker above the prompt is not open")
	}
	if _, ok := parseConfirm(pickerScreen); ok {
		t.Fatal("the picker is no confirmation")
	}
}

// fakeTerm plays Claude Code's prompt, /model picker and "Switch model?"
// confirmation.
type fakeTerm struct {
	mu sync.Mutex
	// rows are the picker's models (labels as shown), efforts[i] the
	// effort labels row i cycles through (nil: none).
	rows    []string
	efforts [][]string
	// model and effort are what the session runs at; cursor and level
	// what the open picker shows.
	model, effort int
	state         string // "prompt", "picker", "confirm", "dialog"
	cursor, level int
	// confirm makes "s" ask "Switch model?" first; deaf makes /model do
	// nothing.
	confirm, deaf bool
	typed         []string
	pressed       []string
}

var efforts6 = []string{"Low", "Medium", "High", "xHigh", "Max", "Ultracode"}

func newFakeTerm() *fakeTerm {
	return &fakeTerm{
		rows:    []string{"Default (recommended)", "Opus (1M context)", "Fable", "Sonnet", "Haiku"},
		efforts: [][]string{efforts6, efforts6, efforts6, efforts6, nil},
		model:   0, effort: 1, state: "prompt",
	}
}

func (f *fakeTerm) Screen(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	b.WriteString("● earlier reply\n  1. a numbered line\n  2. and another\n\n")
	switch f.state {
	case "prompt":
		b.WriteString("────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n")
	case "dialog":
		b.WriteString(" Bash command\n   rm -rf build\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n")
	case "picker":
		b.WriteString("▔▔▔▔\n   Select model\n   Switch between Claude models.\n\n")
		for i, r := range f.rows {
			mark, check := "  ", ""
			if i == f.cursor {
				mark = "❯ "
			}
			if i == f.model {
				check = " ✔"
			}
			fmt.Fprintf(&b, "   %s%d. %-24s Some model · Good for things\n", mark, i+1, r+check)
		}
		if lv := f.efforts[f.cursor]; lv == nil {
			b.WriteString("\n   ○ Effort not supported for " + f.rows[f.cursor] + "\n")
		} else {
			b.WriteString("\n   ◐ " + lv[f.level] + " effort ←/→ to adjust\n")
		}
		b.WriteString("\n   Enter to set as default · s to use this session only · Esc to cancel\n")
	case "confirm":
		b.WriteString("▔▔▔▔\n   Switch model?\n   Your next response will be slower and use more tokens\n" +
			"   This conversation is cached for the current model.\n   ❯ 1. Yes, switch to " + f.rows[f.cursor] + "\n     2. No, go back\n")
	}
	return b.String(), nil
}

func (f *fakeTerm) Type(_ context.Context, text string, submit bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, text)
	if text == "/model" && submit && f.state == "prompt" && !f.deaf {
		f.state, f.cursor, f.level = "picker", f.model, f.effort
	}
	return nil
}

func (f *fakeTerm) Press(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.pressed = append(f.pressed, k)
		n := len(f.rows)
		switch {
		case f.state == "picker" && (k == "Down" || k == "Up"):
			d := 1
			if k == "Up" {
				d = n - 1
			}
			f.cursor = (f.cursor + d) % n
			if lv := f.efforts[f.cursor]; lv != nil {
				f.level = min(f.level, len(lv)-1)
			}
		case f.state == "picker" && (k == "Right" || k == "Left") && f.efforts[f.cursor] != nil:
			levels := len(f.efforts[f.cursor])
			d := 1
			if k == "Left" {
				d = levels - 1
			}
			f.level = (f.level + d) % levels
		case f.state == "picker" && k == "s":
			if f.confirm {
				f.state = "confirm"
			} else {
				f.apply()
			}
		case f.state == "picker" && k == "Escape":
			f.state = "prompt"
		case f.state == "confirm" && k == "Enter":
			f.apply()
		case f.state == "prompt" && (k == "Enter" || k == "Escape"):
			// Enter would send a message; Esc would interrupt a turn.
			return fmt.Errorf("%s pressed at the prompt", k)
		}
	}
	return nil
}

func (f *fakeTerm) apply() {
	f.model, f.state = f.cursor, "prompt"
	if f.efforts[f.cursor] != nil {
		f.effort = f.level
	}
}

func TestSwitchModel(t *testing.T) {
	defer func(w time.Duration) { pickerWait = w }(pickerWait)
	pickerWait = 300 * time.Millisecond
	c := &claude{}
	ctx := context.Background()

	tests := []struct {
		name          string
		setup         func(*fakeTerm)
		model, effort string
		// wantModel and wantEffort are the row and effort level the
		// session ends at; wantErr a part of the error.
		wantModel, wantEffort int
		wantErr               string
	}{
		{name: "model and effort", model: "fable", effort: "high", wantModel: 2, wantEffort: 2},
		{name: "effort only", effort: "ultracode", wantModel: 0, wantEffort: 5},
		{name: "effort down", setup: func(f *fakeTerm) { f.effort = 4 }, effort: "low", wantModel: 0, wantEffort: 0},
		{name: "up to opus", setup: func(f *fakeTerm) { f.model = 3 }, model: "opus", wantModel: 1, wantEffort: 1},
		{name: "haiku has no effort", model: "haiku", effort: "high", wantModel: 4, wantEffort: 1},
		{name: "confirmed", setup: func(f *fakeTerm) { f.confirm = true }, model: "sonnet", effort: "max", wantModel: 3, wantEffort: 4},
		{name: "dialog open", setup: func(f *fakeTerm) { f.state = "dialog" }, model: "sonnet", wantErr: "dialog"},
		{name: "picker never opens", setup: func(f *fakeTerm) { f.deaf = true }, model: "sonnet", wantErr: "did not open"},
		{name: "model not offered", setup: func(f *fakeTerm) {
			f.rows, f.efforts = f.rows[:3], f.efforts[:3]
		}, model: "sonnet", wantErr: "offers no Sonnet"},
		{name: "effort not offered", setup: func(f *fakeTerm) {
			f.efforts[2] = []string{"Low", "Medium", "High"}
		}, model: "fable", effort: "max", wantErr: "does not run at max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTerm()
			if tt.setup != nil {
				tt.setup(f)
			}
			wasState := f.state
			err := c.SwitchModel(ctx, f, tt.model, tt.effort)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err %v, want %q", err, tt.wantErr)
				}
				if f.state != wasState {
					t.Fatalf("left the terminal in %q (keys %q)", f.state, f.pressed)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v (typed %q, pressed %q)", err, f.typed, f.pressed)
			}
			if f.state != "prompt" || f.model != tt.wantModel || f.effort != tt.wantEffort {
				t.Fatalf("state %s, model %d effort %d; want model %d effort %d (pressed %q)",
					f.state, f.model, f.effort, tt.wantModel, tt.wantEffort, f.pressed)
			}
			if slices.Contains(f.pressed, "Enter") != f.confirm {
				t.Fatalf("pressed %q: Enter saves the pick as the default", f.pressed)
			}
		})
	}
}
