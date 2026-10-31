package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Each skill in the shared folder is linked into every agent CLI's skills
// folder: <cli dir>/<name> -> <shared folder>/<name>. What is already there
// under that name stays, unless it is a broken link or leads to the same
// folder: a skill of the user's own is never replaced, it is reported as a
// conflict. Links into the shared folder whose skill is gone are removed.

// refreshDirs looks up the agent CLIs' skills folders again when it is
// time to, without holding s.mu: detecting the CLIs may take a while.
func (s *Store) refreshDirs(ctx context.Context) {
	if s.opts.LinkDirs == nil {
		return
	}
	s.mu.Lock()
	due := time.Since(s.dirsAt) > dirsInterval
	s.mu.Unlock()
	if !due {
		return
	}
	dirs := s.opts.LinkDirs(ctx)
	s.mu.Lock()
	s.dirs, s.dirsAt = dirs, time.Now()
	s.mu.Unlock()
}

// linkLocked updates the links of every skill.
func (s *Store) linkLocked() {
	names := s.localNames()
	for _, dir := range s.dirs {
		if len(names) > 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				s.opts.Log.Warn("skills: creating a skills folder failed", "dir", dir, "err", err)
				continue
			}
		}
		for _, name := range names {
			if err := link(s.opts.Dir, dir, name); err != nil && !errors.Is(err, errConflict) {
				s.opts.Log.Warn("skills: linking a skill failed", "skill", name, "dir", dir, "err", err)
			}
		}
		unlinkGone(s.opts.Dir, dir, names)
	}
}

// localNames lists the skills in the shared folder, broken or not.
func (s *Store) localNames() []string {
	des, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if ValidName(de.Name()) && (de.IsDir() || de.Type()&fs.ModeSymlink != 0) {
			out = append(out, de.Name())
		}
	}
	return out
}

// errConflict means a CLI's skills folder has something else by that name.
var errConflict = errors.New("another skill of that name is there")

// linkState says whether dir/name is the link to shared/name: ok, missing
// (fixable), or a conflict.
func linkState(shared, dir, name string) (ok, fixable bool) {
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		// A broken link.
		return false, fi.Mode()&fs.ModeSymlink != 0
	}
	want, err := filepath.EvalSymlinks(filepath.Join(shared, name))
	return err == nil && got == want, false
}

// link links dir/name to shared/name unless it is already there.
func link(shared, dir, name string) error {
	ok, fixable := linkState(shared, dir, name)
	switch {
	case ok:
		return nil
	case !fixable:
		return errConflict
	}
	p, to := filepath.Join(dir, name), filepath.Join(shared, name)
	if got, err := os.Readlink(p); err == nil && got == to {
		return nil // ours, to a skill whose own link is broken
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(to, p)
}

// unlinkGone removes the links in dir into shared whose skill is not in
// names.
func unlinkGone(shared, dir string, names []string) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.Type()&fs.ModeSymlink == 0 || slices.Contains(names, de.Name()) {
			continue
		}
		p := filepath.Join(dir, de.Name())
		if to, err := os.Readlink(p); err == nil && filepath.Dir(to) == filepath.Clean(shared) {
			os.Remove(p)
		}
	}
}

// Info is how a skill in the shared folder stands, for fleet skill ls.
type Info struct {
	Name string
	// Path is its folder, Target where a linked one lives ("" if not
	// linked).
	Path, Target string
	// Entry is its manifest entry; zero if the daemon has not taken it yet.
	Entry Entry
	// Problem says why it is not shared; Pending that it changed since the
	// daemon last took it.
	Problem string
	Pending bool
	// Links is, for each agent CLI's skills folder, whether the skill is
	// linked there; Conflicts the folders that have another skill by that
	// name.
	Links, Conflicts []string
}

// Status reports the skills in the shared folder dir, the manifest state,
// and their links into dirs, without changing anything (fleet skill ls
// runs it beside a running daemon).
func Status(dir, state string, dirs []string) ([]Info, error) {
	m, err := readManifest(state)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []Info
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || !de.IsDir() && de.Type()&fs.ModeSymlink == 0 {
			continue
		}
		in := Info{Name: name, Path: filepath.Join(dir, name)}
		if !ValidName(name) {
			in.Problem = "not a skill name (letters, digits, - and _), left out"
			out = append(out, in)
			continue
		}
		if de.Type()&fs.ModeSymlink != 0 {
			in.Target, _ = os.Readlink(in.Path)
		}
		if i := slices.IndexFunc(m.Skills, func(e Entry) bool { return e.Name == name }); i >= 0 && !m.Skills[i].Deleted {
			in.Entry = m.Skills[i]
		}
		if files, err := walk(in.Path); err != nil {
			in.Problem = problem(err)
		} else if hash, _, err := hashFiles(files); err != nil {
			in.Problem = problem(err)
		} else {
			in.Pending = hash != in.Entry.Hash
		}
		for _, d := range dirs {
			if ok, fixable := linkState(dir, d, name); ok {
				in.Links = append(in.Links, d)
			} else if !fixable {
				in.Conflicts = append(in.Conflicts, d)
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// How Add put a skill into the shared folder.
const (
	Moved  = "moved"
	Linked = "linked"
	Copied = "copied"
)

// Add puts the skill folder src into the shared folder dir, under src's
// name. One that lives in an agent CLI's skills folder (one of dirs) is
// moved, the daemon's link taking its place; else it is linked with link,
// copied without. Links in dirs that led to src go too, for the daemon to
// link the shared skill there instead; with link they already lead to it.
func Add(dir, src string, dirs []string, link bool) (name, how string, err error) {
	src, err = filepath.Abs(src)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", "", err
	}
	if _, err := walk(real); err != nil {
		return "", "", fmt.Errorf("%s: %s", src, problem(err))
	}
	name = filepath.Base(src)
	if !ValidName(name) {
		return "", "", fmt.Errorf("%q is not a skill name (letters, digits, - and _, at most 64): rename the folder", name)
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		return "", "", fmt.Errorf("a skill called %s is shared already", name)
	}
	if shared, err := filepath.EvalSymlinks(dir); err == nil && within(real, shared) {
		return "", "", fmt.Errorf("%s is in the shared folder already", src)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	parent := filepath.Dir(real)
	inCLI := slices.ContainsFunc(dirs, func(d string) bool {
		r, err := filepath.EvalSymlinks(d)
		return err == nil && r == parent
	})
	switch {
	case inCLI:
		how = Moved
		if err := os.Rename(real, dst); err != nil {
			// Another file system: copy, then remove.
			if err := copyTree(real, dst); err != nil {
				return "", "", err
			}
			if err := os.RemoveAll(real); err != nil {
				return "", "", err
			}
		}
	case link:
		how = Linked
		if err := os.Symlink(real, dst); err != nil {
			return "", "", err
		}
	default:
		how = Copied
		if err := copyTree(real, dst); err != nil {
			return "", "", err
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if to, err := filepath.EvalSymlinks(p); err != nil || to == real && how != Linked {
			os.Remove(p)
		}
	}
	return name, how, nil
}

// Remove deletes skill name from the shared folder dir, and with it from
// every machine of the fleet. Of a linked skill only the link goes.
func Remove(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%q is not a skill name", name)
	}
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); err != nil {
		return fmt.Errorf("no skill called %s is shared", name)
	}
	return remove(path)
}

// within reports whether path is inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}
