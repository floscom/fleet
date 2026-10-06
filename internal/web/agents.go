package web

// Sessions from the browser: start an agent, follow its conversation, type
// into it and stop it. Admin routes like the rest of /api/, and forwarded
// to other fleets holding the same fleet key (see peers.go).
//
//	GET  /api/agents                  every agent, finished ones included
//	POST /api/agents                  start one, images attached to its prompt
//	GET  /api/agents/{id}/chat        its conversation (see serveChat)
//	GET  /api/agents/{id}/screen      its terminal screen, as text
//	POST /api/agents/{id}/input       attach images, type text and/or press keys (see apiInput)
//	GET  /api/agents/{id}/image       an image of its conversation
//	GET  /api/agents/{id}/media       all the images of its conversation (see media.go)
//	POST /api/agents/{id}/answer      answer the questions it asks
//	POST /api/agents/{id}/model       switch its model and/or effort
//	POST /api/agents/{id}/stop        kill it; forget drops it from the list too

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
	"fleet/internal/ask"
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
	// maxImages caps the images of one input request.
	maxImages = 10
	// maxInputBody caps the body of an input or start request, images
	// included (each image is limited again by the daemon).
	maxInputBody = 40 << 20
	// modelTimeout bounds switching a session's model: the daemon types
	// into its terminal and waits for each step to show.
	modelTimeout = 30 * time.Second
)

// Chat is where an agent's conversation is (Source.Chat).
type Chat struct {
	Agent *fleetv1.Agent
	// Path is the transcript file, "" while there is none (yet).
	Path string
	// Parse parses its lines; nil when the adapter keeps no transcript.
	Parse transcript.Parser
	// Model is the model and effort the session runs at; nil when its
	// adapter offers no choice of model.
	Model *Model
	// Image reads image n of a transcript line; nil when the adapter keeps
	// no images.
	Image func(line []byte, n int) (mediaType string, data []byte, ok bool)
	// Asks are the questions the agent waits on, for a form.
	Asks []ask.Pending
}

// Model is the model and effort an agent's session runs at, and what it
// may run at instead.
type Model struct {
	// Model is the ID of one of Models, "" if unknown or none matches; Name
	// is what the CLI calls it, e.g. "claude-opus-5-5".
	Model string `json:"model"`
	Name  string `json:"name"`
	// Effort is the ID of one of Efforts, "" if unknown or none.
	Effort string `json:"effort"`
	// Switch is set when the running session can be switched to another
	// model or effort (POST /api/agents/{id}/model).
	Switch  bool           `json:"switch"`
	Models  []ModelChoice  `json:"models"`
	Efforts []EffortChoice `json:"efforts"`
}

// ModelChoice is a model an adapter offers.
type ModelChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Efforts are the IDs of the efforts it runs at; none if it has no
	// effort levels.
	Efforts []string `json:"efforts"`
}

// EffortChoice is a reasoning effort level an adapter offers.
type EffortChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
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
		Adapter   string   `json:"adapter"`
		Root      string   `json:"root"`
		Path      string   `json:"path"`
		Prompt    string   `json:"prompt"`
		Name      string   `json:"name"`
		Branch    string   `json:"branch"`
		Isolation string   `json:"isolation"` // "", "worktree" or "pinned"
		Sandbox   string   `json:"sandbox"`   // "", "docker" or "none"
		Model     string   `json:"model"`     // "" for the CLI's default
		Effort    string   `json:"effort"`    // "" for the CLI's default
		Images    [][]byte `json:"images"`    // attached to the prompt
	}
	if !decodeMax(w, r, &req, maxInputBody) {
		return
	}
	if len(req.Images) > maxImages {
		writeError(w, http.StatusBadRequest, "too many images (at most "+strconv.Itoa(maxImages)+")")
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
		Model: strings.TrimSpace(req.Model), Effort: strings.TrimSpace(req.Effort),
		Images: req.Images,
	})
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]Agent{"agent": agentOf(a)})
}

// apiStopAgent ends a session (the dashboard's Archive) and with forget
// also removes the agent from the list (Delete), which works for finished
// agents too.
func (s *Server) apiStopAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RemoveWorktree bool `json:"removeWorktree"`
		Force          bool `json:"force"`
		Forget         bool `json:"forget"`
	}
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.opts.Source.StopAgent(r.Context(), &fleetv1.KillAgentRequest{
		Agent: r.PathValue("id"), RemoveWorktree: req.RemoveWorktree, Force: req.Force, Forget: req.Forget,
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

// apiInput attaches images (base64; JSON decodes them), types text, then
// presses Enter if submit, then keys. It answers {"held": true} when the
// agent showed a dialog and Enter was not pressed after the text: in a
// dialog Enter picks the highlighted option, whatever was typed.
func (s *Server) apiInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string   `json:"text"`
		Submit bool     `json:"submit"`
		Keys   []string `json:"keys"`
		Images [][]byte `json:"images"`
	}
	if !decodeMax(w, r, &req, maxInputBody) {
		return
	}
	if len(req.Keys) > maxKeys {
		writeError(w, http.StatusBadRequest, "too many keys")
		return
	}
	if len(req.Images) > maxImages {
		writeError(w, http.StatusBadRequest, "too many images (at most "+strconv.Itoa(maxImages)+")")
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
	held, err := s.opts.Source.SendInput(r.Context(), r.PathValue("id"), req.Text, req.Submit, keys, req.Images)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"held": held})
}

// apiSwitchModel switches a running session to another model and/or
// effort ("" keeps the current one), for that session only, and answers
// {"model": Model}.
func (s *Server) apiSwitchModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelTimeout)
	defer cancel()
	m, err := s.opts.Source.SwitchModel(ctx, r.PathValue("id"), req.Model, req.Effort)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]*Model{"model": m})
}

// apiResume switches auto-resume after a usage limit for an agent
// ({"auto": bool}) and/or resumes it now ({"now": true}).
func (s *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Auto *bool `json:"auto"`
		Now  bool  `json:"now"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Auto == nil && !req.Now {
		writeError(w, http.StatusBadRequest, "auto or now is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelTimeout)
	defer cancel()
	a, err := s.opts.Source.SetAutoResume(ctx, r.PathValue("id"), req.Auto, req.Now)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]Agent{"agent": agentOf(a)})
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
	// Model is the model and effort the session runs at, if its adapter
	// offers a choice.
	Model *Model `json:"model,omitempty"`
	// Asks are the questions the agent waits on, in the order it shows
	// them; answer them with POST .../answer.
	Asks []ask.Pending `json:"asks"`
}

// apiChat serves an agent's conversation, parsed from its transcript.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.serveChat(w, r, func() (Chat, error) { return s.opts.Source.Chat(id) })
}

// serveChat serves the conversation in the transcript find returns (again
// each time a waiting request looks for news):
//
//	(no offsets)         the latest entries (Reset)
//	file=F&after=N       entries after offset N of file F; with wait=1 and
//	                     v=<updatedAtMs the page knows>, held until there
//	                     are some, the agent changed, or chatWait passed
//	                     (or with asks=<ids>, its questions changed)
//	file=F&before=N      entries before offset N (older)
//
// A file that is not the agent's transcript (anymore) gives the latest
// entries of the current one, with Reset.
func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, find func() (Chat, error)) {
	q := r.URL.Query()
	file := q.Get("file")
	after, afterErr := strconv.ParseInt(q.Get("after"), 10, 64)
	before, beforeErr := strconv.ParseInt(q.Get("before"), 10, 64)
	wait := q.Get("wait") == "1" && afterErr == nil
	seen, _ := strconv.ParseInt(q.Get("v"), 10, 64)
	deadline := time.Now().Add(chatWait)
	for {
		c, err := find()
		if err != nil {
			s.writeSourceError(w, err)
			return
		}
		reply := chatReply{Agent: agentOf(c.Agent), File: fileKey(c.Path), Entries: []transcript.Entry{}, Model: c.Model, Asks: nonNil(c.Asks)}
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
			reply = chatReply{Agent: reply.Agent, Entries: []transcript.Entry{}, Reset: file != "", Model: c.Model, Asks: reply.Asks}
			err = nil
		}
		if err != nil {
			s.writeSourceError(w, err)
			return
		}
		news := len(reply.Entries) > 0 || reply.Reset || reply.More || reply.Agent.UpdatedAtMs != seen || q.Has("asks") && asksNews(q.Get("asks"), reply.Asks)
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

// asksNews reports whether the questions differ from those the page knows
// (asks=<ids, comma separated>): a question comes with the dialog that
// asks it, but may also turn up while the agent already waits on another.
func asksNews(known string, asks []ask.Pending) bool {
	ids := make([]string, len(asks))
	for i, a := range asks {
		ids[i] = a.ID
	}
	return strings.Join(ids, ",") != known
}

// apiAnswer answers questions the agent asks (chatReply.Asks):
//
//	{"ask": "<id>", "answers": {"<question>": ["<label or text>", ...]},
//	 "notes": {"<question>": "..."}}  or  {"ask": "<id>", "decline": "..."}
func (s *Server) apiAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ask string `json:"ask"`
		ask.Answer
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Ask == "" {
		writeError(w, http.StatusBadRequest, "ask is required")
		return
	}
	if err := s.opts.Source.Answer(r.PathValue("id"), req.Ask, req.Answer); err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// imageTypes are the image types the page is given.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// apiImage serves an image of an agent's conversation.
func (s *Server) apiImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.serveImage(w, r, func() (Chat, error) { return s.opts.Source.Chat(id) })
}

// serveImage serves image n of the line at offset line of the transcript
// find returns (file=F&line=N&n=K, see transcript.Image) as
// {"type": "image/png", "data": "<base64>"}: JSON, like every reply a
// fleet forwards from another (see peers.go), shown as a data: URL.
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, find func() (Chat, error)) {
	q := r.URL.Query()
	line, err1 := strconv.ParseInt(q.Get("line"), 10, 64)
	n, err2 := strconv.Atoi(q.Get("n"))
	if err1 != nil || err2 != nil || line < 0 || n < 0 {
		writeError(w, http.StatusBadRequest, "line and n are required")
		return
	}
	c, err := find()
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if c.Path == "" || c.Image == nil || q.Get("file") != fileKey(c.Path) {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	data, ok, err := transcript.LineAt(c.Path, line, transcript.MaxLine)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.writeSourceError(w, err)
		return
	}
	var typ string
	var img []byte
	if ok {
		typ, img, ok = c.Image(data, n)
	}
	if !ok || !imageTypes[typ] {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": typ, "data": img})
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
	return decodeMax(w, r, v, maxBody)
}

// decodeMax is decode for bodies up to limit bytes; a larger one is
// answered 413.
func decodeMax(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// bodyLimit is the largest request body accepted for an API path: input
// and start requests carry images.
func bodyLimit(path string) int64 {
	if strings.HasSuffix(path, "/input") || strings.HasSuffix(path, "/agents") {
		return maxInputBody
	}
	return maxBody
}

// fileSize is the size of the file at path, 0 if it cannot be read.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
