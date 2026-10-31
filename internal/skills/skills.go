// Package skills shares agent skills across a fleet.
//
// A skill is a folder with a SKILL.md and whatever it brings along
// (scripts/, references/, assets/, ...), the format Claude Code, Codex and
// Gemini CLI all load. Skills put into <FLEET_HOME>/skills are
//
//   - linked into each agent CLI's own skills folder (adapter.SkillDirer:
//     ~/.claude/skills for Claude Code, ~/.agents/skills for Codex and
//     Gemini CLI), so every CLI on the machine finds them, and
//   - kept the same on every daemon holding the fleet key: each keeps a
//     manifest (skills.json) of its skills by content hash and version, and
//     pulls whichever skill another daemon has a newer version of (the
//     network part is web/skills.go).
//
// A skill folder is the unit: the newer version wins as a whole, nothing
// is merged inside it. A version is the time of the change on the daemon
// that made it, ties broken by daemon id; a change made here always gets a
// version above the one it replaces, so it wins even over a clock running
// ahead elsewhere. Deleting a skill leaves a tombstone in the manifest: the
// deletion travels like a change instead of the skill coming back from a
// machine that still has it.
//
// The folder is scanned every scanInterval, and a change is taken once it
// held still for a scan, so a skill being copied in is not sent half done.
// Left out: folders not named like a skill (backups like
// "git-commit.bak.20260722", hidden ones), folders without a SKILL.md, and
// inside a skill links, VCS folders, caches and editor or OS litter (junk).
// A folder linked in (fleet skill add --link) is followed, and updated in
// place where it lives.
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// scanInterval is how often the folder is scanned.
const scanInterval = 2 * time.Second

// dirsInterval is how often the agent CLIs' skills folders are looked up
// again (a CLI installed meanwhile).
const dirsInterval = time.Minute

// tempPrefix starts the names of fleet's temporary folders; not a valid
// skill name, so a scan never takes one for a skill.
const tempPrefix = ".fleet-"

// ErrNeedData is Apply's error for a version whose files it was not given.
var ErrNeedData = errors.New("skills: the skill's files are needed")

// ErrChanging is Archive's error for a skill whose files changed since its
// last scan: it gets a new version shortly.
var ErrChanging = errors.New("skills: the skill is changing")

// ErrNotFound is Archive's error for a skill this machine does not share.
var ErrNotFound = errors.New("skills: no such skill")

// Entry is a skill in the manifest.
type Entry struct {
	Name string `json:"name"`
	// Hash is the content hash (hashFiles); "" for a deleted skill.
	Hash string `json:"hash,omitempty"`
	// Size is the size of its files in bytes.
	Size int64 `json:"size,omitempty"`
	// Version orders changes: Unix milliseconds on the daemon By that made
	// this one.
	Version int64 `json:"version"`
	// By is the server id of that daemon, Host its name.
	By   string `json:"by"`
	Host string `json:"host,omitempty"`
	// Deleted marks a tombstone.
	Deleted bool `json:"deleted,omitempty"`
}

// Newer reports whether e replaces o.
func (e Entry) Newer(o Entry) bool {
	if e.Version != o.Version {
		return e.Version > o.Version
	}
	return e.By > o.By
}

// manifest is the file the manifest is kept in.
type manifest struct {
	Skills []Entry `json:"skills"`
}

// Options configures a Store.
type Options struct {
	// Dir is the shared folder, State the manifest file.
	Dir, State string
	// ID and Name are this daemon's server id and name, recorded with the
	// changes made here.
	ID, Name string
	// LinkDirs returns the agent CLIs' skills folders to link the skills
	// into; nil links nowhere.
	LinkDirs func(ctx context.Context) []string
	Log      *slog.Logger
}

// Store is the shared skills folder of this machine and its manifest.
type Store struct {
	opts Options

	mu      sync.Mutex
	entries map[string]Entry
	seen    map[string]seen
	dirs    []string
	dirsAt  time.Time
	changed chan struct{}
}

// seen is what the last scan saw of a skill folder.
type seen struct {
	fp string
	// hash is the content hash taken at fp, "" until fp held still for
	// a scan; problem says why the folder is not shared.
	hash    string
	size    int64
	problem string
}

// Open loads the manifest and creates the folder if needed.
func Open(opts Options) (*Store, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	m, err := readManifest(opts.State)
	if err != nil {
		return nil, err
	}
	s := &Store{opts: opts, entries: map[string]Entry{}, seen: map[string]seen{}, changed: make(chan struct{}, 1)}
	for _, e := range m.Skills {
		if ValidName(e.Name) {
			s.entries[e.Name] = e
		}
	}
	// Leftovers of an Apply cut short.
	if des, err := os.ReadDir(opts.Dir); err == nil {
		for _, de := range des {
			if strings.HasPrefix(de.Name(), tempPrefix) {
				os.RemoveAll(filepath.Join(opts.Dir, de.Name()))
			}
		}
	}
	return s, nil
}

func readManifest(path string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Dir is the shared folder.
func (s *Store) Dir() string { return s.opts.Dir }

// Changed is signalled when the manifest changed: a skill changed here or
// came from another machine.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// Run scans the folder and keeps the links until ctx ends.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(scanInterval)
	defer t.Stop()
	for {
		s.Scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Scan records the changes to the folder and updates the links.
func (s *Store) Scan(ctx context.Context) {
	s.refreshDirs(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanLocked(false) {
		s.saveLocked()
		s.signal()
	}
	s.linkLocked()
}

// Manifest is every skill this machine knows of, deleted ones included,
// by name.
func (s *Store) Manifest() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Want returns the entries of another machine's manifest that replace
// what this one has.
func (s *Store) Want(remote []Entry) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, r := range remote {
		if !ValidName(r.Name) || r.By == "" || r.Hash == "" && !r.Deleted {
			continue
		}
		if l, ok := s.entries[r.Name]; !ok || r.Newer(l) {
			out = append(out, r)
		}
	}
	return out
}

// Needs reports whether Apply needs the files of e: it is not a deletion,
// and this machine does not have these files already.
func (s *Store) Needs(e Entry) bool {
	if e.Deleted {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.entries[e.Name]
	return !ok || l.Deleted || l.Hash != e.Hash
}

// Archive returns the manifest entry of skill name and its files, packed
// for Apply on another machine.
func (s *Store) Archive(name string) (Entry, []byte, error) {
	s.mu.Lock()
	e, ok := s.entries[name]
	s.mu.Unlock()
	if !ok || e.Deleted || !ValidName(name) {
		return Entry{}, nil, ErrNotFound
	}
	data, hash, err := pack(filepath.Join(s.opts.Dir, name))
	if err != nil {
		return Entry{}, nil, err
	}
	if hash != e.Hash {
		return Entry{}, nil, ErrChanging
	}
	return e, data, nil
}

// Apply makes skill e.Name what another machine has: e, with the files in
// data (an Archive), which may be nil unless Needs(e). An e that no longer
// replaces what this machine has, say because the skill was just changed
// here, is dropped: applied is false.
func (s *Store) Apply(e Entry, data []byte) (applied bool, err error) {
	if !ValidName(e.Name) {
		return false, fmt.Errorf("skills: %q is not a skill name", e.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A change made here but not taken yet must win over e.
	if s.scanLocked(true) {
		s.saveLocked()
		s.signal()
	}
	l, ok := s.entries[e.Name]
	if ok && !e.Newer(l) {
		return false, nil
	}
	path := filepath.Join(s.opts.Dir, e.Name)
	switch {
	case e.Deleted:
		if err := remove(path); err != nil {
			return false, err
		}
		delete(s.seen, e.Name)
	case ok && !l.Deleted && l.Hash == e.Hash:
		// The same files: only the version is news.
	case data == nil:
		return false, ErrNeedData
	default:
		if err := install(path, data, e.Hash); err != nil {
			return false, fmt.Errorf("skills: %s: %w", e.Name, err)
		}
		if files, err := walk(path); err == nil {
			s.seen[e.Name] = seen{fp: fingerprint(files), hash: e.Hash, size: e.Size}
		} else {
			delete(s.seen, e.Name)
		}
	}
	s.entries[e.Name] = e
	s.saveLocked()
	s.signal()
	s.linkLocked()
	return true, nil
}

// scanLocked records the changes to the folder in the manifest and reports
// whether there were any. Unless now, a changed skill is taken only once
// it held still since the previous scan.
func (s *Store) scanLocked(now bool) bool {
	des, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		s.opts.Log.Warn("skills: reading the shared folder failed", "dir", s.opts.Dir, "err", err)
		return false
	}
	changed := false
	present := map[string]bool{}
	for _, de := range des {
		name := de.Name()
		if !ValidName(name) {
			continue
		}
		// A folder, or a link (even a broken one: the skill is not gone,
		// its folder is just out of reach).
		if !de.IsDir() && de.Type()&fs.ModeSymlink == 0 {
			continue
		}
		present[name] = true
		prev := s.seen[name]
		files, err := walk(filepath.Join(s.opts.Dir, name))
		if err != nil {
			s.seen[name] = seen{problem: problem(err)}
			continue
		}
		fp := fingerprint(files)
		if fp == prev.fp && prev.hash != "" {
			continue
		}
		if fp != prev.fp && !now {
			s.seen[name] = seen{fp: fp}
			continue
		}
		hash, size, err := hashFiles(files)
		if err != nil {
			s.seen[name] = seen{problem: problem(err)}
			continue
		}
		s.seen[name] = seen{fp: fp, hash: hash, size: size}
		if e, ok := s.entries[name]; ok && !e.Deleted && e.Hash == hash {
			continue
		}
		s.entries[name] = s.change(Entry{Name: name, Hash: hash, Size: size})
		changed = true
		s.opts.Log.Info("skills: skill changed here", "skill", name)
	}
	for name, e := range s.entries {
		if !present[name] && !e.Deleted {
			s.entries[name] = s.change(Entry{Name: name, Deleted: true})
			delete(s.seen, name)
			changed = true
			s.opts.Log.Info("skills: skill deleted here", "skill", name)
		}
	}
	for name := range s.seen {
		if !present[name] {
			delete(s.seen, name)
		}
	}
	return changed
}

// change stamps e as a change made here, newer than what it replaces.
func (s *Store) change(e Entry) Entry {
	e.Version = time.Now().UnixMilli()
	if prev, ok := s.entries[e.Name]; ok && prev.Version >= e.Version {
		e.Version = prev.Version + 1
	}
	e.By, e.Host = s.opts.ID, s.opts.Name
	return e
}

func problem(err error) string {
	switch {
	case errors.Is(err, errNoSkill), errors.Is(err, errTooBig):
		return err.Error()
	case errors.Is(err, fs.ErrNotExist):
		return "the folder it links to is missing"
	}
	return err.Error()
}

func (s *Store) saveLocked() {
	m := manifest{Skills: make([]Entry, 0, len(s.entries))}
	for _, e := range s.entries {
		m.Skills = append(m.Skills, e)
	}
	slices.SortFunc(m.Skills, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = writeFileAtomic(s.opts.State, append(b, '\n'))
	}
	if err != nil {
		s.opts.Log.Warn("skills: saving the manifest failed", "err", err)
	}
}

func (s *Store) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// install replaces the skill folder path with the files in data, which
// must have content hash hash. A linked folder is replaced where it lives.
func install(path string, data []byte, hash string) error {
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			// Broken: the link goes, the skill comes in its place.
			if err := os.Remove(path); err != nil {
				return err
			}
		} else {
			target = real
		}
	}
	dir := filepath.Dir(target)
	tmp, err := os.MkdirTemp(dir, tempPrefix+"new-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // no-op once renamed
	got, err := unpack(data, tmp)
	if err != nil {
		return err
	}
	if got != hash {
		return errors.New("the files that came differ from their hash")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	old := ""
	if _, err := os.Lstat(target); err == nil {
		old = filepath.Join(dir, tempPrefix+"old-"+filepath.Base(target)+fmt.Sprint(time.Now().UnixNano()))
		if err := os.Rename(target, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		if old != "" {
			os.Rename(old, target)
		}
		return err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	return nil
}

// remove deletes the skill folder path, or only the link if it is one.
func remove(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// writeFileAtomic writes data to a temp file next to path and renames it
// into place.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
