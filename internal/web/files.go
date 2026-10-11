package web

// The files of a session, for its Files tab:
//
//	GET /api/agents/{id}/files         what it wrote and changed (see apiFiles)
//	GET /api/agents/{id}/file?path=P   one of them, its content (see apiFile)
//
// An agent's files are those its tool calls wrote (transcript.Entry.Files,
// wherever they are) and those of its folder changed since its session
// started: that catches what its commands made too (a build, a rendered
// image). A search looks through the whole folder.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"fleet/internal/transcript"
)

const (
	// filesWalk bounds the entries one look through a folder visits.
	filesWalk = 100_000
	// maxFiles caps the files a reply lists, maxFound its search hits.
	maxFiles = 500
	maxFound = 200
	// maxFile is the largest file served with its content: in base64 it
	// still fits a reply relayed from another fleet (maxPeerReply).
	maxFile = 16 << 20
	// filesScan bounds the transcript bytes one request reads for the
	// files written; the next request reads on.
	filesScan = 256 << 20
	// keepWritten is how many transcripts' files written are remembered.
	keepWritten = 64
)

// skipDirs are folders a look through a folder leaves out: version control,
// packages and caches, many files nobody looks for.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".jj": true,
	"node_modules": true, ".pnpm-store": true, ".yarn": true, "bower_components": true,
	".venv": true, "venv": true, "__pycache__": true, ".tox": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, ".turbo": true, ".parcel-cache": true,
	".cache": true, ".gradle": true, ".terraform": true, "target": true,
}

// File is a file of a session.
type File struct {
	// Path is absolute; Rel is relative to the agent's folder, "" for a
	// file outside it.
	Path string `json:"path"`
	Rel  string `json:"rel,omitempty"`
	Size int64  `json:"size"`
	// MTimeMs is when it last changed, ms since the epoch; for a file gone,
	// when the agent last wrote it (0 if unknown).
	MTimeMs int64 `json:"mtimeMs"`
	// Written is set when a tool call of the agent wrote it.
	Written bool `json:"written,omitempty"`
	// Gone is set when it was written but is not there anymore.
	Gone bool `json:"gone,omitempty"`
}

// filesReply is the answer of GET /api/agents/{id}/files.
type filesReply struct {
	// Dir is the agent's folder, "" if it has none (anymore).
	Dir string `json:"dir"`
	// SinceMs is when its session started: Files has the files of Dir
	// changed since.
	SinceMs int64 `json:"sinceMs"`
	// Files are what the agent wrote and changed, newest first; with q,
	// those whose path holds it.
	Files []File `json:"files"`
	// Found are, with q, the other files of Dir whose path holds it,
	// newest first.
	Found []File `json:"found"`
	// Partial means Dir was not looked through in full, or a list was cut.
	Partial bool `json:"partial"`
}

// apiFiles lists the files an agent wrote and the files of its folder
// changed since its session started; with q=TEXT, only those whose path
// (relative to its folder) holds every word of TEXT, ignoring case, and
// in Found the other files of its folder that do.
func (s *Server) apiFiles(w http.ResponseWriter, r *http.Request) {
	c, err := s.opts.Source.Chat(r.PathValue("id"))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	written, err := s.written.files(c)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	since := c.StartedAtMs
	if since == 0 {
		since = c.Agent.GetCreatedAtMs()
	}
	reply := filesReply{Dir: agentDir(c), SinceMs: since, Files: []File{}, Found: []File{}}
	match := matcher(r.URL.Query().Get("q"))
	byPath := map[string]*File{}
	if reply.Dir != "" {
		reply.Partial = walk(r.Context(), reply.Dir, func(f File) {
			switch {
			case f.MTimeMs >= since:
				byPath[f.Path] = &f
			case match != nil && match(f.Rel):
				reply.Found = append(reply.Found, f)
			}
		})
	}
	for p, ms := range written {
		p = absPath(reply.Dir, p)
		if f := byPath[p]; f != nil {
			f.Written = true
			continue
		}
		f := File{Path: p, Rel: relPath(reply.Dir, p), Written: true}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			f.Size, f.MTimeMs = fi.Size(), fi.ModTime().UnixMilli()
		} else if err == nil {
			continue // a folder, a device: nothing to show
		} else {
			f.Gone, f.MTimeMs = true, ms
		}
		// Found already holds it if it was in the folder but older.
		reply.Found = slices.DeleteFunc(reply.Found, func(g File) bool { return g.Path == p })
		byPath[p] = &f
	}
	for _, f := range byPath {
		if match == nil || match(cmp.Or(f.Rel, f.Path)) {
			reply.Files = append(reply.Files, *f)
		}
	}
	cut := func(list []File, max int) []File {
		sort.Slice(list, func(i, j int) bool {
			if list[i].MTimeMs != list[j].MTimeMs {
				return list[i].MTimeMs > list[j].MTimeMs
			}
			return list[i].Path < list[j].Path
		})
		if len(list) > max {
			reply.Partial = true
			return list[:max]
		}
		return list
	}
	reply.Files = cut(reply.Files, maxFiles)
	reply.Found = cut(reply.Found, maxFound)
	writeJSON(w, http.StatusOK, reply)
}

// fileReply is the answer of GET /api/agents/{id}/file.
type fileReply struct {
	File
	// Type is its media type, by its name or else its content.
	Type string `json:"type"`
	// Text is set when it holds text (UTF-8, no NUL bytes).
	Text bool `json:"text"`
	// Data is its content, base64; nil for a file over maxFile (Big).
	Data []byte `json:"data"`
	Big  bool   `json:"big,omitempty"`
}

// apiFile serves a file of an agent (path=P, absolute): one of its folder,
// or one it wrote. JSON with the content in base64, like every reply a
// fleet forwards from another (see peers.go).
func (s *Server) apiFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	p = filepath.Clean(p)
	c, err := s.opts.Source.Chat(r.PathValue("id"))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	ok, err := s.fileOf(c, p)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not a file of this session")
		return
	}
	// Looked at before it is opened: opening a named pipe would wait for
	// a writer.
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "the file is gone")
		return
	}
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not a regular file")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	defer f.Close()
	dir := agentDir(c)
	reply := fileReply{File: File{Path: p, Rel: relPath(dir, p), Size: fi.Size(), MTimeMs: fi.ModTime().UnixMilli()}}
	limit := int64(maxFile)
	if reply.Big = fi.Size() > maxFile; reply.Big {
		limit = 512 // enough to tell its type
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	reply.Type = mime.TypeByExtension(filepath.Ext(p))
	if reply.Type == "" {
		reply.Type = http.DetectContentType(data)
	}
	reply.Text = isText(data, reply.Big)
	if !reply.Big {
		reply.Data = data
	}
	writeJSON(w, http.StatusOK, reply)
}

// fileOf reports whether the file at p (absolute, clean) is one the
// agent's Files tab may show: in its folder, or written by it. Links are
// followed: one in the folder that leads out of it is not.
func (s *Server) fileOf(c Chat, p string) (bool, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		real = p // gone: only a file it wrote matches below
	}
	if dir := agentDir(c); dir != "" {
		if rd, err := filepath.EvalSymlinks(dir); err == nil && within(rd, real) {
			return true, nil
		}
	}
	written, err := s.written.files(c)
	if err != nil {
		return false, err
	}
	for w := range written {
		if absPath(agentDir(c), w) == p {
			return true, nil
		}
	}
	return false, nil
}

// agentDir is the folder an agent works in, "" if it has none (anymore).
func agentDir(c Chat) string {
	dir := c.Agent.GetCwd()
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Clean(dir)
}

// within reports whether path p is dir or inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// absPath is p as an absolute path: a relative one is in dir.
func absPath(dir, p string) string {
	if !filepath.IsAbs(p) && dir != "" {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// relPath is p relative to dir, "" if it is not inside it.
func relPath(dir, p string) string {
	if dir == "" || !within(dir, p) || p == dir {
		return ""
	}
	rel, _ := filepath.Rel(dir, p)
	return filepath.ToSlash(rel)
}

// walk calls visit for each regular file in dir and its folders, but those
// of skipDirs, and reports whether it stopped early (filesWalk, or ctx
// ended). Folders it may not read are left out.
func walk(ctx context.Context, dir string, visit func(File)) (partial bool) {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if n++; n > filesWalk || n%1000 == 0 && ctx.Err() != nil {
			partial = true
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		visit(File{Path: p, Rel: relPath(dir, p), Size: fi.Size(), MTimeMs: fi.ModTime().UnixMilli()})
		return nil
	})
	return partial
}

// matcher returns a test for paths that hold every word of q, ignoring
// case; nil for an empty q.
func matcher(q string) func(string) bool {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil
	}
	return func(p string) bool {
		p = strings.ToLower(p)
		for _, w := range words {
			if !strings.Contains(p, w) {
				return false
			}
		}
		return true
	}
}

// isText reports whether data looks like text: UTF-8 without NUL bytes.
// A prefix (cut) may end inside a character.
func isText(data []byte, cut bool) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	if cut {
		for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	return utf8.Valid(data)
}

// ---------------------------------------------------------------------------
// Files written, by transcript

// writtenCache remembers the files each transcript's tool calls wrote, so
// a request reads only the lines added since the one before.
type writtenCache struct {
	mu   sync.Mutex
	m    map[string]*written // by transcript path
	tick int64
}

type written struct {
	mu   sync.Mutex
	used int64 // writtenCache.tick when last asked for
	end  int64 // offset after the lines read
	// files maps each path written, as the agent named it, to when it was
	// last written (ms; 0 if unknown).
	files map[string]int64
}

// files returns the files the tool calls in c's transcript wrote, mapped
// to when each was last written. The map is a copy.
func (wc *writtenCache) files(c Chat) (map[string]int64, error) {
	if c.Path == "" || c.Parse == nil {
		return nil, nil
	}
	wc.mu.Lock()
	if wc.m == nil {
		wc.m = map[string]*written{}
	}
	wr := wc.m[c.Path]
	if wr == nil {
		wr = &written{files: map[string]int64{}}
		wc.m[c.Path] = wr
		for len(wc.m) > keepWritten {
			var old string
			for k, v := range wc.m {
				if old == "" || v.used < wc.m[old].used {
					old = k
				}
			}
			delete(wc.m, old)
		}
	}
	wc.tick++
	wr.used = wc.tick
	wc.mu.Unlock()

	wr.mu.Lock()
	defer wr.mu.Unlock()
	err := wr.scan(c)
	if errors.Is(err, transcript.ErrShrunk) {
		wr.end, wr.files = 0, map[string]int64{}
		err = wr.scan(c)
	}
	if errors.Is(err, fs.ErrNotExist) {
		wr.end, wr.files, err = 0, map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(wr.files))
	for p, ms := range wr.files {
		out[p] = ms
	}
	return out, nil
}

// scan reads the transcript on from where the last scan ended, up to about
// filesScan bytes.
func (wr *written) scan(c Chat) error {
	from := wr.end
	for {
		p, err := transcript.Forward(c.Path, wr.end, c.Parse)
		if err != nil {
			return err
		}
		wr.end = p.End
		for _, e := range p.Entries {
			for _, f := range e.Files {
				if f != "" {
					wr.files[f] = max(wr.files[f], e.TimeMs)
				}
			}
		}
		if !p.More || wr.end-from >= filesScan {
			return nil
		}
	}
}
