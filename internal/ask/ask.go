// Package ask holds the questions an agent asks its user in a dialog of
// choices (Claude's AskUserQuestion), and the answers the dashboard gives
// to them in place of keys typed into the dialog.
package ask

import (
	"errors"
	"fmt"
	"slices"
)

// Question is one question of a dialog.
type Question struct {
	// Question is the full question; it also keys its answer.
	Question string `json:"question"`
	// Header is a short label for it, e.g. "Auth method".
	Header string `json:"header,omitempty"`
	// MultiSelect allows more than one answer.
	MultiSelect bool     `json:"multiSelect,omitempty"`
	Options     []Option `json:"options"`
}

// Option is a choice of a question. The user may also type their own.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Preview is content to show while the option is looked at (a mockup,
	// code), as text.
	Preview string `json:"preview,omitempty"`
}

// Pending is a dialog of questions waiting for its answer.
type Pending struct {
	// ID names it for Answer.
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
}

// Answer answers a dialog.
type Answer struct {
	// Answers maps the text of each question to the labels picked or the
	// text typed: one, or several for a MultiSelect question.
	Answers map[string][]string `json:"answers"`
	// Notes maps the text of a question to a note the user added.
	Notes map[string]string `json:"notes,omitempty"`
	// Decline, if set, answers none of the questions: it is what the user
	// wrote back instead.
	Decline string `json:"decline,omitempty"`
}

// Size limits of an answer.
const (
	maxText    = 8 << 10
	maxAnswers = 32 << 10
)

// Check reports whether a answers qs: every question once, with one
// answer (several for a MultiSelect one), and nothing else; or a Decline.
func (a Answer) Check(qs []Question) error {
	if a.Decline != "" {
		if len(a.Decline) > maxText {
			return errors.New("the reply is too long")
		}
		if len(a.Answers) > 0 || len(a.Notes) > 0 {
			return errors.New("a declined question has no answers")
		}
		return nil
	}
	total := 0
	for text, picks := range a.Answers {
		i := slices.IndexFunc(qs, func(q Question) bool { return q.Question == text })
		if i < 0 {
			return fmt.Errorf("no question %q", text)
		}
		if len(picks) == 0 || len(picks) > 1 && !qs[i].MultiSelect {
			return fmt.Errorf("question %q takes one answer", text)
		}
		if len(picks) > len(qs[i].Options)+1 {
			return fmt.Errorf("too many answers to %q", text)
		}
		for _, p := range picks {
			if p == "" || len(p) > maxText {
				return fmt.Errorf("an answer to %q is empty or too long", text)
			}
			total += len(p)
		}
	}
	for _, q := range qs {
		if _, ok := a.Answers[q.Question]; !ok {
			return fmt.Errorf("question %q is not answered", q.Question)
		}
	}
	for text, note := range a.Notes {
		if _, ok := a.Answers[text]; !ok {
			return fmt.Errorf("no question %q", text)
		}
		if len(note) > maxText {
			return fmt.Errorf("the note on %q is too long", text)
		}
		total += len(note)
	}
	if total > maxAnswers {
		return errors.New("the answers are too long")
	}
	return nil
}
