package claude

import (
	"encoding/json"
	"strings"

	"fleet/internal/adapter"
	"fleet/internal/ask"
)

// Questions returns the questions of an AskUserQuestion call, which Claude
// asks in a dialog it reports with PermissionRequest (adapter.Asker).
func (c *claude) Questions(ev adapter.HookEvent) ([]ask.Question, bool) {
	if ev.Event != "PermissionRequest" {
		return nil, false
	}
	var p payload
	if json.Unmarshal(ev.Payload, &p) != nil || p.ToolName != "AskUserQuestion" {
		return nil, false
	}
	var in struct {
		Questions []ask.Question `json:"questions"`
	}
	if json.Unmarshal(p.ToolInput, &in) != nil || len(in.Questions) == 0 {
		return nil, false
	}
	for _, q := range in.Questions {
		if q.Question == "" {
			return nil, false
		}
	}
	return in.Questions, true
}

// AnswerOutput answers a PermissionRequest for AskUserQuestion. Claude
// takes the answers as the call's input, next to the questions:
//
//	{"questions": [...], "answers": {"<question>": "<label or text>"},
//	 "annotations": {"<question>": {"notes": "..."}}}
//
// A MultiSelect question's answer is a list. A decline denies the call
// with the user's reply, as the dialog's "Chat about this" does.
// Verified against Claude Code 2.1.280.
func (c *claude) AnswerOutput(ev adapter.HookEvent, a ask.Answer) ([]byte, error) {
	var p payload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	decision := map[string]any{}
	if a.Decline != "" {
		decision["behavior"] = "deny"
		decision["message"] = "The user did not answer the questions and wrote instead:\n\n" + strings.TrimSpace(a.Decline)
	} else {
		var input map[string]any
		if err := json.Unmarshal(p.ToolInput, &input); err != nil {
			return nil, err
		}
		multi := map[string]bool{}
		if qs, ok := c.Questions(ev); ok {
			for _, q := range qs {
				multi[q.Question] = q.MultiSelect
			}
		}
		answers := map[string]any{}
		for q, picks := range a.Answers {
			if multi[q] {
				answers[q] = picks
			} else if len(picks) > 0 {
				answers[q] = picks[0]
			}
		}
		input["answers"] = answers
		if len(a.Notes) > 0 {
			notes := map[string]any{}
			for q, n := range a.Notes {
				if n = strings.TrimSpace(n); n != "" {
					notes[q] = map[string]string{"notes": n}
				}
			}
			input["annotations"] = notes
		}
		decision["behavior"] = "allow"
		decision["updatedInput"] = input
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PermissionRequest",
		"decision":      decision,
	}})
}
