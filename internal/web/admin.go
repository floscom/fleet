package web

// Admin API: the folder picker and root management behind /api/; the
// routes for agents are in agents.go.
//
// Viewing the dashboard needs nothing; changing it needs the admin token
// that `fleet web` prints as a link. The page keeps the token in
// localStorage, which is per origin (port included, unlike cookies), and
// sends it as "Authorization: Bearer <token>". No cookie means no ambient
// credential, so cross-site requests (CSRF) carry nothing that works.
//
// The token lives in a file (Options.TokenPath) and is read on every
// admin request, so `fleet web --rotate` signs every browser out at once
// without restarting the daemon. Other daemons holding the same fleet key
// reach the same routes with signed requests (peers.go). Someone who knows
// the token or the fleet key can also type it into the page, which trades
// it for the token at POST /api/unlock.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

const (
	// maxDirEntries caps a folder listing.
	maxDirEntries = 2000
	// maxBody caps request bodies.
	maxBody = 64 << 10
	// adaptersTimeout bounds adapter detection (it runs the CLIs).
	adaptersTimeout = 5 * time.Second
)

// Error is a Source error shown to the user with an HTTP status.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// Dir is a folder listing for the root picker.
type Dir struct {
	// Path is absolute and symlink-resolved.
	Path string `json:"path"`
	// Parent is "" at the filesystem root.
	Parent  string     `json:"parent"`
	Home    string     `json:"home"`
	Entries []DirEntry `json:"entries"`
	// Truncated is set when entries were cut at maxDirEntries.
	Truncated bool `json:"truncated"`
}

// DirEntry is a subfolder.
type DirEntry struct {
	Name string `json:"name"`
	// Git is set when the folder has a .git entry.
	Git bool `json:"git"`
}

// Adapter is an agent adapter a root can be limited to.
type Adapter struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// ---------------------------------------------------------------------------
// Token

// LoadOrCreateToken returns the admin token stored at path, creating it
// (mode 0600) if the file is missing or empty.
func LoadOrCreateToken(path string) (string, error) {
	if tok, err := readToken(path); err == nil && tok != "" {
		return tok, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return RotateToken(path)
}

// RotateToken stores a new random admin token at path and returns it.
// Browsers holding the old one lose admin rights on their next request.
func RotateToken(path string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if err := writeFileAtomic(path, []byte(tok+"\n")); err != nil {
		return "", fmt.Errorf("write web token: %w", err)
	}
	return tok, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeFileAtomic writes data to a 0600 temp file next to path and renames
// it into place.
func writeFileAtomic(path string, data []byte) error {
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
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// hasToken reports whether r carries the current admin token.
func (s *Server) hasToken(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && holds(s.opts.TokenPath, got)
}

// holds reports whether got is the non-empty secret stored at path.
func holds(path, got string) bool {
	if path == "" || got == "" {
		return false
	}
	want, err := readToken(path)
	if err != nil || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---------------------------------------------------------------------------
// Routes

// apiHandler routes /api/. /api/session, /api/nonce and /api/unlock are
// open; the rest needs the admin token, or a request signed with the fleet
// key (see peers.go), which reaches this daemon's own routes only.
func (s *Server) apiHandler() http.Handler {
	admin := http.NewServeMux()
	admin.HandleFunc("GET /api/fs", s.apiListDir)
	admin.HandleFunc("GET /api/adapters", s.apiAdapters)
	admin.HandleFunc("GET /api/roots", s.apiRoots)
	admin.HandleFunc("POST /api/roots", s.apiAddRoot)
	admin.HandleFunc("DELETE /api/roots/{name}", s.apiRemoveRoot)
	admin.HandleFunc("GET /api/agents", s.apiAgents)
	admin.HandleFunc("POST /api/agents", s.apiRunAgent)
	admin.HandleFunc("GET /api/agents/{id}/chat", s.apiChat)
	admin.HandleFunc("GET /api/agents/{id}/screen", s.apiScreen)
	admin.HandleFunc("POST /api/agents/{id}/input", s.apiInput)
	admin.HandleFunc("POST /api/agents/{id}/stop", s.apiStopAgent)
	browser := http.NewServeMux()
	browser.HandleFunc("GET /api/join", s.apiJoin)
	browser.HandleFunc("/api/hosts/{id}/{rest...}", s.apiHost)
	browser.Handle("/", admin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/api/nonce":
			s.apiNonce(w, r)
			return
		case "/api/unlock":
			s.apiUnlock(w, r)
			return
		}
		token := s.hasToken(r)
		peer := false
		if !token && isPeerRequest(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			peer = s.peerAuthed(r)
		}
		switch {
		case r.URL.Path == "/api/session":
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"admin": token || peer})
		case token:
			browser.ServeHTTP(w, r)
		case peer:
			admin.ServeHTTP(w, r)
		case isPeerRequest(r):
			writeError(w, http.StatusUnauthorized, "not signed with this fleet's key")
		default:
			writeError(w, http.StatusUnauthorized, "admin token required: run `fleet web` on the server")
		}
	})
}

// apiUnlock trades a secret typed into the page for the admin token: the
// token itself, or the fleet key. The fleet key is worth admin rights here
// anyway, since it signs requests to this daemon (peers.go). Both are 256
// random bits, so guessing through this open route is hopeless.
func (s *Server) apiUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.opts.TokenPath == "" {
		writeError(w, http.StatusNotFound, "this fleet has no admin access")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	key := strings.TrimSpace(req.Key)
	if !holds(s.opts.TokenPath, key) && !holds(s.opts.KeyPath, key) {
		writeError(w, http.StatusForbidden, "that is neither this fleet's key nor its admin token")
		return
	}
	tok, err := LoadOrCreateToken(s.opts.TokenPath)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *Server) apiRoots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]Root{"roots": rootsOf(s.opts.Source.Roots())})
}

func (s *Server) apiListDir(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, err := listDir(q.Get("path"), q.Get("hidden") == "1")
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dir)
}

func (s *Server) apiAdapters(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), adaptersTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string][]Adapter{"adapters": nonNil(s.opts.Source.Adapters(ctx))})
}

func (s *Server) apiAddRoot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string   `json:"path"`
		Name     string   `json:"name"`
		Adapters []string `json:"adapters"`
		Trust    bool     `json:"trust"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	root, err := s.opts.Source.AddRoot(&fleetv1.AddRootRequest{
		Path: req.Path, Name: strings.TrimSpace(req.Name), Adapters: req.Adapters, Trust: req.Trust,
	})
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]Root{"root": rootsOf([]*fleetv1.Root{root})[0]})
}

func (s *Server) apiRemoveRoot(w http.ResponseWriter, r *http.Request) {
	if err := s.opts.Source.RemoveRoot(r.PathValue("name")); err != nil {
		s.writeSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSourceError sends an *Error as is and hides anything else.
func (s *Server) writeSourceError(w http.ResponseWriter, err error) {
	var e *Error
	if errors.As(err, &e) {
		writeError(w, e.Status, e.Msg)
		return
	}
	s.opts.Log.Warn("web ui: request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// Folder listing

// listDir lists the subfolders of path ("" = the home directory), for
// picking a new root anywhere on this machine. Symlinks to folders are
// listed like folders.
func listDir(path string, hidden bool) (Dir, error) {
	home, _ := os.UserHomeDir()
	if path == "" {
		path = home
		if path == "" {
			path = string(filepath.Separator)
		}
	}
	if !filepath.IsAbs(path) {
		return Dir{}, &Error{http.StatusBadRequest, fmt.Sprintf("%q is not an absolute path", path)}
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return Dir{}, fsError(path, err)
	}
	if fi, err := os.Stat(real); err != nil {
		return Dir{}, fsError(real, err)
	} else if !fi.IsDir() {
		return Dir{}, &Error{http.StatusBadRequest, real + " is not a folder"}
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return Dir{}, fsError(real, err)
	}
	out := Dir{Path: real, Home: home, Entries: []DirEntry{}}
	if parent := filepath.Dir(real); parent != real {
		out.Parent = parent
	}
	for _, e := range ents {
		name := e.Name()
		if !hidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(real, name)
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
				continue
			}
		}
		_, gitErr := os.Lstat(filepath.Join(full, ".git"))
		out.Entries = append(out.Entries, DirEntry{Name: name, Git: gitErr == nil})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := strings.ToLower(out.Entries[i].Name), strings.ToLower(out.Entries[j].Name)
		if a != b {
			return a < b
		}
		return out.Entries[i].Name < out.Entries[j].Name
	})
	if len(out.Entries) > maxDirEntries {
		out.Entries, out.Truncated = out.Entries[:maxDirEntries], true
	}
	return out, nil
}

// fsError maps a filesystem error on path to an *Error.
func fsError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Error{http.StatusNotFound, path + ": no such folder"}
	case errors.Is(err, fs.ErrPermission):
		return &Error{http.StatusForbidden, path + ": permission denied"}
	}
	return &Error{http.StatusBadRequest, err.Error()}
}
