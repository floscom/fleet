package web

// One folder of a session's folder, for the tree of its Files tab:
//
//	GET /api/agents/{id}/dir?path=P   the entries of folder P (see apiDir)
//
// The tree reads a folder when it is opened, so a large one costs nothing
// until then. Each entry carries its git status, and a folder whether
// something inside it changed.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// foldProbe bounds the subfolders looked into to fold a chain of
	// folders, foldDepth how long a chain gets.
	foldProbe = 300
	foldDepth = 10
	// gitStatusTimeout bounds the git commands of one reply.
	gitStatusTimeout = 5 * time.Second
)

// vcsDirs are the folders of version control, which the tree leaves out.
var vcsDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, ".jj": true}

// TreeEntry is an entry of a folder.
type TreeEntry struct {
	// Name is its name; for a chain of folders each holding only the next,
	// their names joined by "/" ("src/main/java").
	Name string `json:"name"`
	// Path is absolute: for a chain, the last folder's.
	Path    string `json:"path"`
	Dir     bool   `json:"dir,omitempty"`
	Empty   bool   `json:"empty,omitempty"` // a folder with nothing in it
	Link    bool   `json:"link,omitempty"`  // a symbolic link
	Size    int64  `json:"size,omitempty"`
	MTimeMs int64  `json:"mtimeMs,omitempty"`
	// Git is its git status: M modified, A added, D deleted, R renamed,
	// U untracked, I ignored; "" for none.
	Git string `json:"git,omitempty"`
	// Changed marks a folder with changes inside.
	Changed bool `json:"changed,omitempty"`
	// Gone is set for a file git knows that is not there anymore.
	Gone bool `json:"gone,omitempty"`
}

// dirReply is the answer of GET /api/agents/{id}/dir.
type dirReply struct {
	// Dir is the agent's folder, Path the folder listed.
	Dir     string      `json:"dir"`
	Path    string      `json:"path"`
	Entries []TreeEntry `json:"entries"`
	// Git is set when Path is in a git work tree.
	Git     bool `json:"git"`
	Partial bool `json:"partial"`
}

// apiDir lists folder path=P (absolute; the agent's folder without it),
// which must be in the agent's folder: its folders first, then its files,
// each by name.
func (s *Server) apiDir(w http.ResponseWriter, r *http.Request) {
	c, err := s.opts.Source.Chat(r.PathValue("id"))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	root := agentDir(c)
	if root == "" {
		writeError(w, http.StatusNotFound, "the session has no folder")
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		p = root
	}
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	p = filepath.Clean(p)
	real, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "the folder is gone")
		return
	}
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if rr, err := filepath.EvalSymlinks(root); err != nil || !within(rr, real) {
		writeError(w, http.StatusNotFound, "not a folder of this session")
		return
	}
	des, err := os.ReadDir(p)
	if err != nil && len(des) == 0 {
		if errors.Is(err, fs.ErrPermission) {
			writeError(w, http.StatusForbidden, "the folder may not be read")
			return
		}
		s.writeSourceError(w, err)
		return
	}
	reply := dirReply{Dir: root, Path: p, Entries: []TreeEntry{}, Partial: err != nil}
	byName := map[string]int{}
	for _, de := range des {
		if vcsDirs[de.Name()] {
			continue
		}
		e := TreeEntry{Name: de.Name(), Path: filepath.Join(p, de.Name())}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			e.Link = true
			if fi, err = os.Stat(e.Path); err != nil {
				continue // leads nowhere
			}
		}
		switch {
		case fi.IsDir():
			e.Dir = true
		case fi.Mode().IsRegular():
			e.Size = fi.Size()
		default:
			continue // a device, a socket, a pipe: nothing to show
		}
		e.MTimeMs = fi.ModTime().UnixMilli()
		byName[e.Name] = len(reply.Entries)
		reply.Entries = append(reply.Entries, e)
	}

	ctx, cancel := context.WithTimeout(r.Context(), gitStatusTimeout)
	defer cancel()
	if st, ok := gitStatusIn(ctx, p); ok {
		reply.Git = true
		for name, g := range st.direct {
			if i, ok := byName[name]; ok {
				reply.Entries[i].Git = g
			} else if g == "D" {
				byName[name] = len(reply.Entries)
				reply.Entries = append(reply.Entries, TreeEntry{Name: name, Path: filepath.Join(p, name), Git: g, Gone: true})
			}
		}
		for name := range st.inside {
			if i, ok := byName[name]; ok {
				reply.Entries[i].Changed = true
			} else {
				// A folder whose files were all deleted.
				byName[name] = len(reply.Entries)
				reply.Entries = append(reply.Entries, TreeEntry{Name: name, Path: filepath.Join(p, name), Dir: true, Git: "D", Gone: true})
			}
		}
		if st.all != "" {
			for i := range reply.Entries {
				if reply.Entries[i].Git == "" {
					reply.Entries[i].Git = st.all
				}
			}
		}
	}

	sort.Slice(reply.Entries, func(i, j int) bool {
		a, b := reply.Entries[i], reply.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
	if len(reply.Entries) > maxDirEntries {
		reply.Entries, reply.Partial = reply.Entries[:maxDirEntries], true
	}
	probed := 0
	for i := range reply.Entries {
		e := &reply.Entries[i]
		if !e.Dir || e.Gone || e.Link || probed >= foldProbe {
			continue
		}
		probed++
		fold(e)
	}
	writeJSON(w, http.StatusOK, reply)
}

// fold looks into folder e: one with nothing in it is Empty; one holding
// only a folder becomes the chain of both, and so on.
func fold(e *TreeEntry) {
	for range foldDepth {
		des, err := os.ReadDir(e.Path)
		if err != nil {
			return
		}
		if len(des) == 0 {
			e.Empty = true
			return
		}
		if len(des) > 1 || !des[0].IsDir() {
			return
		}
		e.Name += "/" + des[0].Name()
		e.Path = filepath.Join(e.Path, des[0].Name())
	}
}

// gitStatus is the git status of a folder's entries.
type gitStatus struct {
	// direct maps an entry's name to its status; inside holds the names
	// of folders with changes inside.
	direct map[string]string
	inside map[string]bool
	// all is the status of the folder itself, untracked or ignored, which
	// is that of everything in it.
	all string
}

// gitStatusIn reads the git status of folder dir, reporting false when it
// is not in a git work tree or git fails.
func gitStatusIn(ctx context.Context, dir string) (gitStatus, bool) {
	st := gitStatus{direct: map[string]string{}, inside: map[string]bool{}}
	prefix, err := readGitOut(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return st, false
	}
	prefix = strings.TrimSpace(prefix)
	out, err := readGitOut(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignored=matching", "--", ".")
	if err != nil {
		return st, false
	}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		xy, path := rec[:2], rec[3:]
		if strings.ContainsAny(xy, "RC") {
			i++ // the path it came from
		}
		g := gitCode(xy)
		if prefix != "" && strings.HasSuffix(path, "/") && strings.HasPrefix(prefix, path) {
			st.all = g // the folder listed, or one around it
			continue
		}
		rel, ok := strings.CutPrefix(path, prefix)
		if !ok || rel == "" {
			continue
		}
		name, rest, _ := strings.Cut(rel, "/")
		if rest == "" {
			st.direct[name] = g
		} else if g != "I" {
			st.inside[name] = true
		}
	}
	return st, true
}

// gitCode is the one letter for a porcelain status XY.
func gitCode(xy string) string {
	switch {
	case xy == "??":
		return "U"
	case xy == "!!":
		return "I"
	case strings.ContainsAny(xy, "RC"):
		return "R"
	case strings.Contains(xy, "D") && !strings.Contains(xy, "U"):
		return "D"
	case strings.Contains(xy, "A") && !strings.Contains(xy, "U"):
		return "A"
	}
	return "M"
}

// readGitOut runs a git command that only reads, in dir, and returns its
// output. Like the daemon's, it takes no optional lock from under the
// agent's own git commands.
func readGitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}
