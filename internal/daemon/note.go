package daemon

import (
	"strings"
	"unicode/utf8"

	fleetv1 "fleet/gen/fleetv1"
)

// maxNote caps an agent's note, in characters.
const maxNote = 500

// setNote sets or, with an empty note, clears the note on an agent, live
// or finished, and returns the agent. It leaves UpdatedAtMs alone: a note
// is no news from the agent, and the lists are sorted by it.
func (m *manager) setNote(ref, note string) (*fleetv1.Agent, error) {
	note = strings.TrimSpace(note)
	if !utf8.ValidString(note) {
		return nil, errf(codeInvalid, "note is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(note); n > maxNote {
		return nil, errf(codeInvalid, "note is %d characters long, at most %d are kept", n, maxNote)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.find(ref)
	if err != nil {
		return nil, err
	}
	if a.Note != note {
		a.Note = note
		m.saveLocked()
		m.broadcastLocked(&fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a.proto()}})
	}
	return a.proto(), nil
}
