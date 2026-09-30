package web

// Sessions from the browser: start an agent, follow its conversation, type
// into it and stop it. Admin routes like the rest of /api/, and forwarded
// to other fleets holding the same fleet key (see peers.go).
//
//	GET  /api/agents                  every agent, finished ones included
//	POST /api/agents                  start one
//	GET  /api/agents/{id}/chat        its conversation (see apiChat)
//	GET  /api/agents/{id}/screen      its terminal screen, as text
//	POST /api/agents/{id}/input       type text and/or press keys (see apiInput)
//	POST /api/agents/{id}/stop        kill it

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

const (
	// chatWait is how long a chat request with wait=1 holds out for news.
	// It stays below peerTimeout so a forwarded wait still gets an answer.
	chatWait = 8 * time.Second
	// chatPoll is how often a waiting chat request looks for news.
	chatPoll = 300 * time.Millisecond
	// runTimeout bounds starting an agent (a worktree, a container).
	runTimeout = 75 * time.Second
	// maxKeys caps the keys of one input request.
	maxKeys = 32
)

// Chat is where an agent's conversation is (Source.Chat).
type Chat struct {
	Agent *fleetv1.Agent
	// Path is the transcript file, "" while there is none (yet).
	Path string
	// Parse parses its lines; nil when the adapter keeps no transcript.
	Parse transcript.Parser
}

// keyNames maps the key names the page sends to tmux key names.
var keyNames = map[string]string{
	"enter": "Enter", "escape": "Escape", "tab": "Tab", "shift-tab": "BTab",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"backspace": "BSpace", "space": "Space", "ctrl-c": "C-c", "ctrl-d": "C-d",
	"0": "0", "1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
	"y": "y", "n": "n",
}

func (s *Server) apiAgents(w http.ResponseWriter, r *http.Request) {
	list := s.opts.Source.Agents()
	out := make([]Agent, 0, len(list))
	for _, a := range list {
		out = append(out, agentOf(a))
	}
	writeJSON(w, http.StatusOK, map[string][]Agent{"agents": out})
}

func (s *Server) apiRunAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Adapter   string `json:"adapter"`
		Root      string `json:"root"`
		Path      string `json:"path"`
		Prompt    string `json:"prompt"`
		Name      string `json:"name"`
		Branch    string `json:"branch"`
		Isolation string `json:"isolation"` // "", "worktree" or "pinned"
		Sandbox   string `json:"sandbox"`   // "", "docker" or "none"
	}
	if !decode(w, r, &req) {
		return
	}
	iso, ok := map[string]fleetv1.Isolation{
		"": fleetv1.Isolation_ISOLATION_UNSPECIFIED, "auto": fleetv1.Isolation_ISOLATION_UNSPECIFIED,
		"worktree": fleetv1.Isolation_ISOLATION_WORKTREE, "pinned": fleetv1.Isolation_ISOLATION_PINNED,
	}[req.Isolation]
	if !ok {
		writeError(w, http.StatusBadRequest, "isolation is worktree or pinned")
		return
	}
	sb, ok := map[string]fleetv1.Sandbox{
		"": fleetv1.Sandbox_SANDBOX_UNSPECIFIED, "default": fleetv1.Sandbox_SANDBOX_UNSPECIFIED,
		"docker": fleetv1.Sandbox_SANDBOX_DOCKER, "none": fleetv1.Sandbox_SANDBOX_NONE,
	}[req.Sandbox]
	if !ok {
		writeError(w, http.StatusBadRequest, "sandbox is docker or none")
		return
	}
	if req.Adapter == "" || req.Root == "" {
		writeError(w, http.StatusBadRequest, "adapter and root are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
	defer cancel()
	a, err := s.opts.Source.RunAgent(ctx, &fleetv1.RunAgentRequest{
		Adapter: req.Adapter, Root: req.Root, Path: strings.Trim(req.Path, "/"),
		Name: strings.TrimSpace(req.Name), Branch: strings.TrimSpace(req.Branch),
		Prompt: req.Prompt, Isolation: iso, Sandbox: sb,
	})
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]Agent{"agent": agentOf(a)})
}

func (s *Server) apiStopAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RemoveWorktree bool `json:"removeWorktree"`
		Force          bool `json:"force"`
	}
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.opts.Source.StopAgent(r.Context(), &fleetv1.KillAgentRequest{
		Agent: r.PathValue("id"), RemoveWorktree: req.RemoveWorktree, Force: req.Force,
	})
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent":        agentOf(resp.GetAgent()),
		"worktreeKept": resp.GetWorktreeKept(),
		"reason":       resp.GetWorktreeKeptReason(),
	})
}

// apiInput types text, then presses Enter if submit, then keys. It answers
// {"held": true} when the agent showed a dialog and Enter was not pressed
// after the text: in a dialog Enter picks the highlighted option, whatever
// was typed.
func (s *Server) apiInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string   `json:"text"`
		Submit bool     `json:"submit"`
		Keys   []string `json:"keys"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Keys) > maxKeys {
		writeError(w, http.StatusBadRequest, "too many keys")
		return
	}
	keys := make([]string, 0, len(req.Keys))
	for _, k := range req.Keys {
		name, ok := keyNames[k]
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown key "+strconv.Quote(k))
			return
		}
		keys = append(keys, name)
	}
	held, err := s.opts.Source.SendInput(r.Context(), r.PathValue("id"), req.Text, req.Submit, keys)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"held": held})
}

func (s *Server) apiScreen(w http.ResponseWriter, r *http.Request) {
	screen, err := s.opts.Source.Screen(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	// Rows are padded to the terminal width; the page needs none of that.
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	writeJSON(w, http.StatusOK, map[string]string{"screen": strings.TrimRight(strings.Join(lines, "\n"), "\n")})
}

// chatReply is the answer of GET /api/agents/{id}/chat.
type chatReply struct {
	Agent Agent `json:"agent"`
	// File names the transcript the offsets are in, "" while there is none.
	File    string             `json:"file"`
	Entries []transcript.Entry `json:"entries"`
	// Start and End are the offsets of what was read: pass End as "after"
	// for newer entries, Start as "before" for older ones (Start > 0).
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	// Reset means Entries replace what the page shows: the first read, or
	// the transcript changed (a new session after /clear).
	Reset bool `json:"reset"`
	// More means newer entries are waiting: ask again right away.
	More bool `json:"more"`
}

// apiChat serves an agent's conversation, parsed from its transcript.
//
//	(no offsets)         the latest entries (Reset)
//	file=F&after=N       entries after offset N of file F; with wait=1 and
//	                     v=<updatedAtMs the page knows>, held until there
//	                     are some, the agent changed, or chatWait passed
//	file=F&before=N      entries before offset N (older)
//
// A file that is not the agent's transcript (anymore) gives the latest
// entries of the current one, with Reset.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, file := r.PathValue("id"), q.Get("file")
	after, afterErr := strconv.ParseInt(q.Get("after"), 10, 64)
	before, beforeErr := strconv.ParseInt(q.Get("before"), 10, 64)
	wait := q.Get("wait") == "1" && afterErr == nil
	seen, _ := strconv.ParseInt(q.Get("v"), 10, 64)
	deadline := time.Now().Add(chatWait)
	for {
		c, err := s.opts.Source.Chat(id)
		if err != nil {
			s.writeSourceError(w, err)
			return
		}
		reply := chatReply{Agent: agentOf(c.Agent), File: fileKey(c.Path), Entries: []transcript.Entry{}}
		switch {
		case c.Path == "":
			reply.Reset = file != ""
		case file == reply.File && beforeErr == nil && before >= 0:
			err = reply.read(transcript.Backward(c.Path, before, c.Parse))
		case file == reply.File && afterErr == nil && after >= 0:
			err = reply.read(transcript.Forward(c.Path, after, c.Parse))
		default:
			err = reply.tail(c)
		}
		if errors.Is(err, transcript.ErrShrunk) {
			err = reply.tail(c)
		}
		if errors.Is(err, fs.ErrNotExist) {
			reply = chatReply{Agent: reply.Agent, Entries: []transcript.Entry{}, Reset: file != ""}
			err = nil
		}
		if err != nil {
			s.writeSourceError(w, err)
			return
		}
		news := len(reply.Entries) > 0 || reply.Reset || reply.More || reply.Agent.UpdatedAtMs != seen
		if !wait || news || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, reply)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(chatPoll):
		}
	}
}

// read fills the reply from a page.
func (c *chatReply) read(p transcript.Page, err error) error {
	if err != nil {
		return err
	}
	c.Entries, c.Start, c.End, c.More = nonNil(p.Entries), p.Start, p.End, p.More
	return nil
}

// tail fills the reply with the latest entries of the transcript.
func (c *chatReply) tail(src Chat) error {
	c.Reset = true
	return c.read(transcript.Backward(src.Path, fileSize(src.Path), src.Parse))
}

// fileKey names a transcript without giving its path away.
func fileKey(path string) string {
	if path == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:6])
}

// decode reads a JSON request body into v, or answers 400.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// fileSize is the size of the file at path, 0 if it cannot be read.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
