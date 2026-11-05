package skills

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Skills from a team. The gateway daemon, the one that talks to the team's
// hub, installs and updates them here with InstallTeam and deletes them
// with RemoveTeam; the fleet sync takes them to the other daemons like any
// change, Source and all. Only the gateway changes a team skill as such: a
// change made to its files anywhere in the fleet shows as a content hash
// other than the one installed (Source.Hash), and the scan that sees it
// forks the skill (scanLocked), so that it is the user's own from then on.
// The installs and syncs themselves never fork one: they record the hash
// of the files they put there.

// ErrForked is InstallTeam's error for a team skill that was changed here:
// it is the user's own now, and stays as it is.
var ErrForked = errors.New("skills: the team skill was forked")

// Source is where a skill installed from a team came from.
type Source struct {
	// Team is the team's id, TeamName what this fleet calls it.
	Team     string `json:"team"`
	TeamName string `json:"team_name,omitempty"`
	// Bundle is the id of the bundle the skill comes in, the same for the
	// life of the skill; BundleVersion the version installed.
	Bundle        string `json:"bundle"`
	BundleVersion int64  `json:"bundle_version"`
	// Owner is the member id of its publisher, OwnerName their name.
	Owner     string `json:"owner"`
	OwnerName string `json:"owner_name,omitempty"`
	// Name is the skill's name in the bundle, which it is installed under
	// unless that is taken.
	Name string `json:"name"`
	// Hash is the content hash of the files as installed: files that hash
	// otherwise were changed here.
	Hash string `json:"hash"`
}

// Ref is how the team knows the skill: <owner>/<name>.
func (src Source) Ref() string { return cmp.Or(src.OwnerName, src.Owner) + "/" + src.Name }

// InstallTeam installs or updates the skill in bundle src.Bundle of team
// src.Team: name is its name in the bundle, data its files packed the way
// Archive packs them. An update keeps the name the skill was installed
// under. A new skill gets name unless that is taken, by a skill in the
// shared folder (another member's included) or one of the user's own in an
// agent CLI's skills folder; then <owner>-<name>, then that with a number.
// Under a name not its own, the skill's SKILL.md gets the name it is
// installed under, as the CLIs go by the name in its front matter.
//
// The install is a change made here: it gets a new version, and so goes
// to every machine of the fleet. A bundle version older than the one
// installed changes nothing; a skill forked from the bundle neither
// (ErrForked). src.Name and src.Hash are set here.
func (s *Store) InstallTeam(ctx context.Context, src Source, name string, data []byte) (installed string, err error) {
	if src.Team == "" || src.Bundle == "" {
		return "", errors.New("skills: a team skill needs its team and bundle")
	}
	s.refreshDirs(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	// A change made here but not taken yet forks the skill before an update
	// could replace it.
	if s.scanLocked(true) {
		s.saveLocked()
		s.signal()
	}
	prev, ok := s.teamLocked(src.Team, src.Bundle)
	switch {
	case ok && prev.Source == nil:
		return prev.Name, ErrForked
	case ok && src.BundleVersion < prev.Source.BundleVersion:
		return prev.Name, nil
	case ok:
		installed = prev.Name
	default:
		if installed, err = s.freeNameLocked(name, cmp.Or(src.OwnerName, src.Owner)); err != nil {
			return "", err
		}
	}
	src.Name = name
	var size int64
	path := filepath.Join(s.opts.Dir, installed)
	err = install(path, func(dir string) error {
		if _, err := unpack(data, dir); err != nil {
			return err
		}
		if installed != name {
			if err := renameSkill(dir, installed); err != nil {
				return err
			}
		}
		// The hash as a scan takes it: without junk the archive may have
		// brought along.
		files, err := walk(dir)
		if err != nil {
			return err
		}
		src.Hash, size, err = hashFiles(files)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("skills: %s: %w", installed, err)
	}
	if files, err := walk(path); err == nil {
		s.seen[installed] = seen{fp: fingerprint(files), hash: src.Hash, size: size}
	} else {
		delete(s.seen, installed)
	}
	if !ok || prev.Hash != src.Hash || *prev.Source != src {
		s.entries[installed] = s.change(Entry{Name: installed, Hash: src.Hash, Size: size, Source: &src})
		s.saveLocked()
		s.signal()
		s.opts.Log.Info("skills: team skill installed", "skill", installed, "from", src.Ref(), "team", cmp.Or(src.TeamName, src.Team))
	}
	s.linkLocked()
	return installed, nil
}

// RemoveTeam deletes the skill in bundle bundle of team team, on every
// machine of the fleet, like a deletion made here. A skill forked from the
// bundle stays: ErrNotFound, as for one not installed.
func (s *Store) RemoveTeam(team, bundle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanLocked(true) {
		s.saveLocked()
		s.signal()
	}
	e, ok := s.teamLocked(team, bundle)
	if !ok || e.Source == nil {
		return ErrNotFound
	}
	if err := remove(filepath.Join(s.opts.Dir, e.Name)); err != nil {
		return err
	}
	delete(s.seen, e.Name)
	s.entries[e.Name] = s.change(Entry{Name: e.Name, Deleted: true, Source: e.Source})
	s.saveLocked()
	s.signal()
	s.linkLocked()
	return nil
}

// TeamSkills lists the skills that came from team, "" for every team:
// those installed, those forked, and the tombstones of those deleted, by
// name.
func (s *Store) TeamSkills(team string) []Entry {
	var out []Entry
	for _, e := range s.Manifest() {
		src := cmp.Or(e.Source, e.ForkedFrom)
		if src != nil && (team == "" || src.Team == team) {
			out = append(out, e)
		}
	}
	return out
}

// teamLocked finds the skill installed from bundle bundle of team team, or
// forked from it.
func (s *Store) teamLocked(team, bundle string) (Entry, bool) {
	var fork Entry
	forked := false
	for _, e := range s.entries {
		switch {
		case e.Deleted:
		case e.Source != nil && e.Source.Team == team && e.Source.Bundle == bundle:
			return e, true
		case e.ForkedFrom != nil && e.ForkedFrom.Team == team && e.ForkedFrom.Bundle == bundle:
			fork, forked = e, true
		}
	}
	return fork, forked
}

// freeNameLocked picks the name a new team skill called name, published by
// owner, is installed under.
func (s *Store) freeNameLocked(name, owner string) (string, error) {
	base := cmp.Or(skillName(name), "skill")
	tries := []string{base}
	if o := skillName(owner); o != "" {
		tries = append(tries, cut(o+"-"+base, maxName))
	}
	last := tries[len(tries)-1]
	for i := 2; i < 100; i++ {
		n := "-" + strconv.Itoa(i)
		tries = append(tries, cut(last, maxName-len(n))+n)
	}
	for _, n := range tries {
		if !s.takenLocked(n) {
			return n, nil
		}
	}
	return "", fmt.Errorf("skills: no free name for %s", base)
}

// takenLocked reports whether a new skill cannot be called name: a skill
// has the name here, or something is in its place in the shared folder or
// an agent CLI's skills folder (link.go).
func (s *Store) takenLocked(name string) bool {
	if !ValidName(name) {
		return true
	}
	if e, ok := s.entries[name]; ok && !e.Deleted {
		return true
	}
	if _, err := os.Lstat(filepath.Join(s.opts.Dir, name)); !errors.Is(err, fs.ErrNotExist) {
		return true
	}
	for _, d := range s.dirs {
		if ok, fixable := linkState(s.opts.Dir, d, name); !ok && !fixable {
			return true
		}
	}
	return false
}

// skillName makes s a skill name: runs of other characters become a -, and
// it is cut to the longest a name may be. It is "" if no letter or digit is
// left.
func skillName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	return cut(strings.TrimLeft(b.String(), "-_"), maxName)
}

// cut shortens name to at most n bytes, without a - at the end.
func cut(name string, n int) string {
	if len(name) > n {
		name = name[:n]
	}
	return strings.TrimRight(name, "-")
}

// renameSkill sets the name in the front matter of the SKILL.md in the
// skill folder dir.
func renameSkill(dir, name string) error {
	p := filepath.Join(dir, skillFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out := setName(string(b), name)
	if out == string(b) {
		return nil
	}
	return os.WriteFile(p, []byte(out), 0o644)
}

var (
	// fenceRE is a line that opens or closes YAML front matter; ... closes
	// one too.
	fenceRE = regexp.MustCompile(`^(---|\.\.\.)[ \t]*\r?\n?$`)
	// nameKeyRE starts the top-level name key of front matter.
	nameKeyRE = regexp.MustCompile(`^(name|"name"|'name')[ \t]*:([ \t]|\r?\n?$)`)
)

// setName sets the name in the YAML front matter of SKILL.md text to name,
// which ValidName allows, and leaves every other byte as it is: the name
// key's line is replaced (and the indented lines of a value continued on
// them), or one is added as the front matter's first line. Text without
// front matter is returned as it is: the CLIs go by the folder's name then.
func setName(text, name string) string {
	lines := strings.SplitAfter(text, "\n")
	first := strings.TrimPrefix(lines[0], "\ufeff")
	if !strings.HasPrefix(first, "---") || !fenceRE.MatchString(first) {
		return text
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if fenceRE.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return text
	}
	eol := "\n"
	if strings.HasSuffix(lines[0], "\r\n") {
		eol = "\r\n"
	}
	at, to := 1, 1 // the lines to replace
	for i := 1; i < end; i++ {
		if !nameKeyRE.MatchString(lines[i]) {
			continue
		}
		if strings.HasSuffix(lines[i], "\r\n") {
			eol = "\r\n"
		} else {
			eol = "\n"
		}
		at, to = i, i+1
		// A value continued on indented lines, blank ones among them.
		for j := i + 1; j < end; j++ {
			if l := lines[j]; strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
				if strings.TrimSpace(l) != "" {
					to = j + 1
				}
			} else if strings.TrimSpace(l) != "" {
				break
			}
		}
		break
	}
	// Quoted: a name like 2026-10-01 or 1e3 is not a string to YAML.
	line := `name: "` + name + `"` + eol
	return strings.Join(lines[:at], "") + line + strings.Join(lines[to:], "")
}
