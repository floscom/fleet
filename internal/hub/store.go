package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fleet/internal/team"
)

// store is the data directory.
type store struct{ dir string }

func openStore(dir string) (*store, error) {
	if dir == "" {
		return nil, errors.New("hub: no data directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "teams"), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &store{dir: dir}, nil
}

// teamState is one team's index, teams/<id>/team.json. Everything in it is
// public to the team's members or derived from their requests; names and
// contents are sealed before they get here.
type teamState struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	// Rosters is the whole chain, seq 1 first.
	Rosters []*team.Roster `json:"rosters"`
	// Keys are the sealed team keys of the current epoch.
	Keys []team.SealedKey `json:"keys"`
	// Invites by id (hash of the credential).
	Invites map[string]inviteRec `json:"invites"`
	// Joins are pending join requests by the joiner's member id.
	Joins map[string]team.JoinRequest `json:"joins"`
	// Left are members who left and are still on the roster.
	Left []string `json:"left"`
	// Revoked are member ids no longer served, with the time: members
	// removed by a roster or who left.
	Revoked map[string]time.Time `json:"revoked"`
	// Heads is the current head per bundle id.
	Heads map[string]*team.Head `json:"heads"`
	// Blobs is what is in data/, by ref.
	Blobs map[string]blobRec `json:"blobs"`
	// Events is the log (the last maxEvents), NextEvent the seq of the next.
	Events    []team.Event `json:"events"`
	NextEvent uint64       `json:"next_event"`

	// wake is closed and replaced when an event is appended; long polls
	// wait on it.
	wake chan struct{}
}

type inviteRec struct {
	Expires time.Time `json:"expires"`
	Used    bool      `json:"used,omitempty"`
}

type blobRec struct {
	Size     int64     `json:"size"`
	Uploaded time.Time `json:"uploaded"`
	By       string    `json:"by"`
}

// head is the current roster.
func (t *teamState) head() *team.Roster { return t.Rosters[len(t.Rosters)-1] }

// used is the team's stored data in bytes.
func (t *teamState) used() int64 {
	var n int64
	for _, b := range t.Blobs {
		n += b.Size
	}
	return n
}

// referenced is the set of blob refs live heads point to.
func (t *teamState) referenced() map[string]bool {
	refs := map[string]bool{}
	for _, h := range t.Heads {
		if !h.Deleted {
			refs[h.DataRef] = true
		}
	}
	return refs
}

func newTeamState(id string, now time.Time) *teamState {
	return &teamState{
		ID: id, Created: now,
		Invites: map[string]inviteRec{}, Joins: map[string]team.JoinRequest{}, Revoked: map[string]time.Time{},
		Heads: map[string]*team.Head{}, Blobs: map[string]blobRec{}, NextEvent: 1, wake: make(chan struct{}),
	}
}

func (s *store) teamDir(id string) string  { return filepath.Join(s.dir, "teams", id) }
func (s *store) teamFile(id string) string { return filepath.Join(s.teamDir(id), "team.json") }
func (s *store) blobPath(id, ref string) string {
	return filepath.Join(s.teamDir(id), "data", ref)
}

// loadTeams reads every teams/*/team.json.
func (s *store) loadTeams() (map[string]*teamState, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "teams"))
	if err != nil {
		return nil, err
	}
	teams := map[string]*teamState{}
	for _, e := range entries {
		if !e.IsDir() || !team.ValidID(e.Name()) {
			continue
		}
		data, err := os.ReadFile(s.teamFile(e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // a team directory without an index: never finished creating
		}
		if err != nil {
			return nil, err
		}
		t := newTeamState(e.Name(), time.Time{})
		if err := json.Unmarshal(data, t); err != nil {
			return nil, fmt.Errorf("%s: %w", s.teamFile(e.Name()), err)
		}
		if t.ID != e.Name() || len(t.Rosters) == 0 {
			return nil, fmt.Errorf("%s: index does not match its directory", s.teamFile(e.Name()))
		}
		teams[t.ID] = t
	}
	return teams, nil
}

// saveTeam writes a team's index atomically.
func (s *store) saveTeam(t *teamState) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.teamFile(t.ID), append(data, '\n'))
}

// putBlob stores data under ref.
func (s *store) putBlob(teamID, ref string, data []byte) error {
	return writeFileAtomic(s.blobPath(teamID, ref), data)
}

// getBlob reads a blob.
func (s *store) getBlob(teamID, ref string) ([]byte, error) {
	return os.ReadFile(s.blobPath(teamID, ref))
}

// deleteBlob removes a blob; a missing file is fine.
func (s *store) deleteBlob(teamID, ref string) error {
	err := os.Remove(s.blobPath(teamID, ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// writeFileAtomic writes data to a 0600 temp file next to path, syncs it
// and renames it into place, creating the parent directory (0700) if
// needed.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
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
	return os.Rename(tmp, path)
}
