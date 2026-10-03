// Package web serves the daemon's dashboard: the embedded single page under
// static/, a WebSocket (/ws) that streams this daemon's agents, paired
// devices, roots and the fleet daemons seen on the LAN, and a small admin
// API under /api/ (see admin.go) to browse this machine's folders, add or
// remove roots, and start, follow, type into and stop agents (agents.go),
// here or on other fleets holding the same fleet key (see peers.go).
//
// It serves plain HTTP. Viewing needs no auth; the admin API needs the
// token `fleet web` prints. Requests must name this machine in their Host
// (an IP, localhost or its hostname, see allowedHost) so other sites cannot
// reach it through DNS rebinding, and the WebSocket keeps coder/websocket's
// same-origin check.
//
// The hub holds ONE agent subscription for the whole server, not one per
// browser tab: it keeps the current state and fans out JSON messages to
// every WebSocket client. A client whose buffer fills up is dropped, never
// waited for. Devices and LAN peers are only polled while a client is
// connected.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/ask"
	"fleet/internal/discovery"
	"fleet/internal/workflow"
)

const (
	writeTimeout    = 5 * time.Second
	shutdownTimeout = 2 * time.Second
	devicePoll      = 2 * time.Second
	peerPoll        = 10 * time.Second
	browseTimeout   = 3 * time.Second
	// peerMisses is how many consecutive browses may miss a peer before it
	// leaves the list.
	peerMisses = 2
	// clientBuffer is the per-client message backlog beyond the snapshot.
	clientBuffer = 256
)

const csp = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

// Source is what the dashboard shows; the daemon implements it.
type Source interface {
	// Info describes this daemon.
	Info() Info
	// SubscribeAgents streams AgentUpserted events for every agent, then
	// SnapshotDone, then live AgentUpserted, AgentRemoved and RootsChanged.
	// The channel is closed if the subscriber was dropped as too slow.
	SubscribeAgents() (events <-chan *fleetv1.Event, cancel func())
	// Devices lists the paired devices.
	Devices() []Device
	// Roots lists the configured roots.
	Roots() []*fleetv1.Root
	// AddRoot adds a root. Errors meant for the user are *Error.
	AddRoot(req *fleetv1.AddRootRequest) (*fleetv1.Root, error)
	// RemoveRoot removes a root by name. Errors meant for the user are *Error.
	RemoveRoot(name string) error
	// Adapters lists the agent adapters.
	Adapters(ctx context.Context) []Adapter

	// Agents lists every agent, finished ones included.
	Agents() []*fleetv1.Agent
	// RunAgent starts an agent. Errors meant for the user are *Error, here
	// and below.
	RunAgent(ctx context.Context, req *fleetv1.RunAgentRequest) (*fleetv1.Agent, error)
	// StopAgent kills an agent's session.
	StopAgent(ctx context.Context, req *fleetv1.KillAgentRequest) (*fleetv1.KillAgentResponse, error)
	// SendInput attaches images to a live agent's prompt, types text into
	// its terminal, then Enter if submit, then presses keys (tmux key
	// names). While the agent shows a dialog, images are refused and Enter
	// after text is held back (it would pick the dialog's highlighted
	// option), which held reports.
	SendInput(ctx context.Context, agent, text string, submit bool, keys []string, images [][]byte) (held bool, err error)
	// Screen is the visible terminal screen of a live agent.
	Screen(ctx context.Context, agent string) (string, error)
	// Chat finds an agent's transcript.
	Chat(agent string) (Chat, error)
	// Workflows returns an agent and the workflow runs of its session.
	Workflows(agent string) (*fleetv1.Agent, []workflow.Run, error)
	// WorkflowChat finds the transcript of agent sub of an agent's
	// workflow run run.
	WorkflowChat(agent, run, sub string) (Chat, error)
	// SwitchModel switches a live agent's session to another model and/or
	// effort ("" keeps the current one), for that session only.
	SwitchModel(ctx context.Context, agent, model, effort string) (*Model, error)
	// Answer answers the questions id the agent waits on (Chat.Asks).
	Answer(agent, id string, a ask.Answer) error
}

// BrowseFunc finds fleet daemons on the LAN (discovery.Browse).
type BrowseFunc func(ctx context.Context, timeout time.Duration) ([]discovery.Found, error)

// Options configures New.
type Options struct {
	// Addr is the listen address, e.g. "0.0.0.0:7421".
	Addr   string
	Source Source
	// TokenPath is the admin token file (see LoadOrCreateToken). Empty
	// disables the admin API.
	TokenPath string
	// KeyPath is the fleet key file: daemons holding the same key manage
	// each other through their dashboards (see peers.go). Empty disables it.
	KeyPath string
	Log     *slog.Logger
	// Browse defaults to discovery.Browse. Tests replace it.
	Browse BrowseFunc
}

// Server is the dashboard's HTTP server.
type Server struct {
	opts Options
	// host is the literal host of Addr, accepted as a Host header.
	host string
	// hostname is this machine's hostname (first label), accepted as the
	// first label of a Host header.
	hostname string
	hub      *hub
	wg       sync.WaitGroup
	// nonces and peerHTTP serve requests between daemons (peers.go).
	nonces   nonces
	peerHTTP *http.Client
}

// CheckAddr returns an error unless addr is host:port.
func CheckAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("web address %q: %w", addr, err)
	}
	return nil
}

// New validates opts.Addr and returns a server that is not listening yet.
func New(opts Options) (*Server, error) {
	if err := CheckAddr(opts.Addr); err != nil {
		return nil, err
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Browse == nil {
		opts.Browse = discovery.Browse
	}
	host, _, _ := net.SplitHostPort(opts.Addr)
	s := &Server{opts: opts, host: strings.ToLower(host), peerHTTP: newPeerClient()}
	if hn, err := os.Hostname(); err == nil {
		s.hostname = firstLabel(hn)
	}
	s.hub = newHub(opts.Source, opts.Browse, opts.Log)
	return s, nil
}

// Serve serves on ln (listening on Addr) until ctx is cancelled, then
// shuts the server down and closes every WebSocket.
func (s *Server) Serve(ctx context.Context, ln net.Listener) {
	s.opts.Log.Info("web ui", "url", "http://"+ln.Addr().String()+"/")
	s.start(ctx)
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.opts.Log.Warn("web ui stopped", "err", err)
	}
	// Shutdown does not track hijacked WebSocket connections: the hub
	// closes them when ctx ends; wait for that.
	s.wg.Wait()
}

// start runs the hub until ctx ends.
func (s *Server) start(ctx context.Context) {
	s.hub.ctx = ctx
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.hub.run(ctx)
	}()
}

// handler routes /ws, /api/ and the static files, behind the Host check
// and the security headers.
func (s *Server) handler() http.Handler {
	files := http.FileServer(http.FS(staticFS))
	api := s.apiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		if !s.allowedHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/ws":
			s.serveWS(w, r)
		default:
			files.ServeHTTP(w, r)
		}
	})
}

// allowedHost reports whether the Host header names this machine: an IP,
// localhost, the listen host, or a name whose first label is the hostname
// ("zerox", "zerox.local", "zerox.tailnet.ts.net"). Anything else is a
// foreign name pointed at us (DNS rebinding).
func (s *Server) allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	switch {
	case host == "localhost", host == s.host, net.ParseIP(host) != nil:
		return true
	case s.hostname != "" && firstLabel(host) == s.hostname:
		return true
	}
	return false
}

// firstLabel returns the lowercased first DNS label of name.
func firstLabel(name string) string {
	label, _, _ := strings.Cut(strings.ToLower(name), ".")
	return label
}

// serveWS streams the snapshot and then live messages to one browser. The
// browser never sends; CloseRead handles its close frame.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil) // same-origin check stays on
	if err != nil {
		return // Accept wrote the response
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(s.hub.ctx)
	c := s.hub.join(ctx)
	if c == nil {
		conn.Close(websocket.StatusGoingAway, "server stopping")
		return
	}
	defer s.hub.leave(c)
	for {
		select {
		case b, ok := <-c.send:
			if !ok { // dropped as slow, or the server stops
				conn.Close(websocket.StatusGoingAway, c.reason)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Messages (server -> browser). Timestamps are ms since the epoch; slices
// are never nil so they encode as [].

// Info describes this daemon ("hello").
type Info struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	StartedAtMs int64  `json:"startedAtMs"`
	// Listen is the effective TLS listen address, or "off".
	Listen string `json:"listen"`
	// MDNS reports whether the daemon advertises itself.
	MDNS bool `json:"mdns"`
}

// Device is a paired device.
type Device struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	PairedAtMs   int64  `json:"pairedAtMs"`
	LastSeenAtMs int64  `json:"lastSeenAtMs"`
	Connected    bool   `json:"connected"`
}

// Peer is a fleet daemon on the LAN.
type Peer struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Host  string   `json:"host"`
	Addrs []string `json:"addrs"`
	Port  int      `json:"port"`
	// WebPort is the port of the peer's dashboard, 0 if it does not
	// advertise one.
	WebPort int  `json:"webPort"`
	Self    bool `json:"self"`
}

// Root is an allowed folder.
type Root struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Adapters []string `json:"adapters"`
	Trust    bool     `json:"trust"`
}

// Agent is an agent, with enums as lowercase names.
type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Adapter         string `json:"adapter"`
	Path            string `json:"path"`
	Root            string `json:"root"`
	Cwd             string `json:"cwd"`
	Isolation       string `json:"isolation"`
	Branch          string `json:"branch"`
	State           string `json:"state"`
	StateDetail     string `json:"stateDetail"`
	ExitCode        *int32 `json:"exitCode"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	UpdatedAtMs     int64  `json:"updatedAtMs"`
	AttachedClients int32  `json:"attachedClients"`
	Sandbox         string `json:"sandbox"`
	CloneURL        string `json:"cloneUrl"`
}

// enumName turns AGENT_STATE_NEEDS_INPUT into "needs_input".
func enumName(s fmt.Stringer, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), prefix))
}

func agentOf(a *fleetv1.Agent) Agent {
	out := Agent{
		ID: a.Id, Name: a.Name, Adapter: a.Adapter, Path: a.Path, Root: a.Root, Cwd: a.Cwd,
		Isolation:   enumName(a.Isolation, "ISOLATION_"),
		Branch:      a.Branch,
		State:       enumName(a.State, "AGENT_STATE_"),
		StateDetail: a.StateDetail,
		CreatedAtMs: a.CreatedAtMs, UpdatedAtMs: a.UpdatedAtMs,
		AttachedClients: a.AttachedClients,
		Sandbox:         enumName(a.Sandbox, "SANDBOX_"),
		CloneURL:        a.CloneUrl,
	}
	if a.HasExitCode {
		code := a.ExitCode
		out.ExitCode = &code
	}
	return out
}

func rootsOf(roots []*fleetv1.Root) []Root {
	out := make([]Root, 0, len(roots))
	for _, r := range roots {
		out = append(out, Root{Name: r.Name, Path: r.Path, Adapters: nonNil(r.Adapters), Trust: r.Trust})
	}
	return out
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// encode builds {"type":typ,key:v} (key may be empty for no payload).
func encode(typ, key string, v any) []byte {
	b := []byte(`{"type":"` + typ + `"`)
	if key != "" {
		p, err := json.Marshal(v)
		if err != nil {
			panic(err) // only plain structs are encoded
		}
		b = append(append(b, `,"`+key+`":`...), p...)
	}
	return append(b, '}')
}

// ---------------------------------------------------------------------------
// Hub

// client is one WebSocket connection's outbox.
type client struct {
	send chan []byte
	// reason is the close reason once send is closed.
	reason string
}

type agentEntry struct {
	createdAtMs int64
	msg         []byte
}

type peerEntry struct {
	peer   Peer
	misses int
}

type hub struct {
	src    Source
	browse BrowseFunc
	log    *slog.Logger
	// ctx is the server's lifetime, set before run.
	ctx context.Context
	// ready is closed once the first agent snapshot is in.
	ready     chan struct{}
	readyOnce sync.Once

	mu      sync.Mutex
	closed  bool
	clients map[*client]struct{}
	conns   sync.WaitGroup
	// stopPoll ends the device and peer pollers (running while clients > 0).
	stopPoll context.CancelFunc
	agents   map[string]agentEntry
	roots    []byte
	devices  []byte
	peers    []byte
	peerSet  map[string]*peerEntry
	// browsed is set once the first mDNS browse finished; "peers" messages
	// carry it as "complete" so the browser does not take the LAN list
	// arriving after a self-only list for newly added fleets.
	browsed bool
}

func newHub(src Source, browse BrowseFunc, log *slog.Logger) *hub {
	return &hub{
		src: src, browse: browse, log: log,
		ready:   make(chan struct{}),
		clients: map[*client]struct{}{},
		agents:  map[string]agentEntry{},
		peerSet: map[string]*peerEntry{},
	}
}

// run follows the agent subscription (resubscribing if dropped) until ctx
// ends, then closes every client and waits for their handlers.
func (h *hub) run(ctx context.Context) {
	for ctx.Err() == nil {
		// Subscribe first, then read the roots: the subscription's snapshot
		// has no roots, and any later change is queued on it.
		events, cancel := h.src.SubscribeAgents()
		roots := encode("roots", "roots", rootsOf(h.src.Roots()))
		h.mu.Lock()
		if !bytes.Equal(h.roots, roots) {
			h.roots = roots
			h.broadcastLocked(roots)
		}
		h.mu.Unlock()
		h.follow(ctx, events)
		cancel()
		if ctx.Err() == nil {
			h.log.Warn("web ui: agent subscription dropped, resubscribing")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		h.dropLocked(c, "server stopping")
	}
	h.mu.Unlock()
	h.conns.Wait()
}

// follow applies events until the channel closes or ctx ends. Agents that
// are missing from a (re)subscription's snapshot are removed.
func (h *hub) follow(ctx context.Context, events <-chan *fleetv1.Event) {
	seen := map[string]bool{}
	for {
		var ev *fleetv1.Event
		var ok bool
		select {
		case <-ctx.Done():
			return
		case ev, ok = <-events:
			if !ok {
				return
			}
		}
		h.mu.Lock()
		switch k := ev.Kind.(type) {
		case *fleetv1.Event_AgentUpserted:
			a := agentOf(k.AgentUpserted)
			if seen != nil {
				seen[a.ID] = true
			}
			msg := encode("agent", "agent", a)
			if prev, ok := h.agents[a.ID]; !ok || !bytes.Equal(prev.msg, msg) {
				h.agents[a.ID] = agentEntry{createdAtMs: a.CreatedAtMs, msg: msg}
				h.broadcastLocked(msg)
			}
		case *fleetv1.Event_AgentRemoved:
			h.removeAgentLocked(k.AgentRemoved)
		case *fleetv1.Event_RootsChanged:
			h.roots = encode("roots", "roots", rootsOf(k.RootsChanged.GetRoots()))
			h.broadcastLocked(h.roots)
		case *fleetv1.Event_SnapshotDone:
			for id := range h.agents {
				if seen != nil && !seen[id] {
					h.removeAgentLocked(id)
				}
			}
			seen = nil
			h.readyOnce.Do(func() { close(h.ready) })
		}
		h.mu.Unlock()
	}
}

func (h *hub) removeAgentLocked(id string) {
	if _, ok := h.agents[id]; ok {
		delete(h.agents, id)
		h.broadcastLocked(encode("agentRemoved", "id", id))
	}
}

// join registers a client, its outbox pre-filled with the snapshot. It
// returns nil if the server stops first.
func (h *hub) join(ctx context.Context) *client {
	select {
	case <-h.ready:
	case <-ctx.Done():
		return nil
	}
	hello := encode("hello", "server", h.src.Info())
	devices := encode("devices", "devices", nonNil(h.src.Devices()))
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.setDevicesLocked(devices)
	if h.peers == nil {
		h.peers = h.peersMsgLocked()
	}
	entries := make([]agentEntry, 0, len(h.agents))
	for _, e := range h.agents {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].createdAtMs != entries[j].createdAtMs {
			return entries[i].createdAtMs < entries[j].createdAtMs
		}
		return bytes.Compare(entries[i].msg, entries[j].msg) < 0
	})
	c := &client{send: make(chan []byte, len(entries)+5+clientBuffer)}
	for _, b := range [][]byte{hello, h.devices, h.peers, h.roots} {
		c.send <- b
	}
	for _, e := range entries {
		c.send <- e.msg
	}
	c.send <- encode("snapshotDone", "", nil)
	h.clients[c] = struct{}{}
	h.conns.Add(1)
	if len(h.clients) == 1 {
		pctx, cancel := context.WithCancel(h.ctx)
		h.stopPoll = cancel
		go h.pollDevices(pctx)
		go h.pollPeers(pctx)
	}
	return c
}

// leave unregisters c (its handler ended).
func (h *hub) leave(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		h.dropLocked(c, "")
	}
	h.conns.Done()
}

// dropLocked unregisters c and closes its outbox; the last client stops
// the pollers.
func (h *hub) dropLocked(c *client, reason string) {
	delete(h.clients, c)
	c.reason = reason
	close(c.send)
	if len(h.clients) == 0 && h.stopPoll != nil {
		h.stopPoll()
		h.stopPoll = nil
	}
}

// broadcastLocked queues msg for every client, dropping those that are
// too far behind.
func (h *hub) broadcastLocked(msg []byte) {
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			h.log.Warn("web ui: dropping slow client")
			h.dropLocked(c, "too slow")
		}
	}
}

func (h *hub) setDevicesLocked(msg []byte) {
	if !bytes.Equal(h.devices, msg) {
		h.devices = msg
		h.broadcastLocked(msg)
	}
}

// pollDevices re-reads the paired devices until ctx ends.
func (h *hub) pollDevices(ctx context.Context) {
	t := time.NewTicker(devicePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		msg := encode("devices", "devices", nonNil(h.src.Devices()))
		h.mu.Lock()
		h.setDevicesLocked(msg)
		h.mu.Unlock()
	}
}

// pollPeers browses mDNS now and then every peerPoll until ctx ends. A peer
// stays listed until peerMisses consecutive browses missed it.
func (h *hub) pollPeers(ctx context.Context) {
	t := time.NewTicker(peerPoll)
	defer t.Stop()
	for {
		found, err := h.browse(ctx, browseTimeout)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Debug("web ui: mdns browse failed", "err", err)
		}
		h.mu.Lock()
		if err == nil {
			h.updatePeersLocked(found)
		}
		h.browsed = true
		if msg := h.peersMsgLocked(); !bytes.Equal(h.peers, msg) {
			h.peers = msg
			h.broadcastLocked(msg)
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *hub) updatePeersLocked(found []discovery.Found) {
	seen := map[string]bool{}
	for _, f := range found {
		key := f.ServerID
		if key == "" {
			key = "instance:" + f.Instance
		}
		seen[key] = true
		h.peerSet[key] = &peerEntry{peer: Peer{
			ID: f.ServerID, Name: f.Name, Host: f.Host, Addrs: nonNil(f.Addrs), Port: f.Port, WebPort: f.WebPort,
		}}
	}
	for key, e := range h.peerSet {
		if !seen[key] {
			if e.misses++; e.misses >= peerMisses {
				delete(h.peerSet, key)
			}
		}
	}
}

// peer returns the fleet daemon id seen on the LAN, never this one.
func (h *hub) peer(id string) (Peer, bool) {
	if id == "" || id == h.src.Info().ID {
		return Peer{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.peerSet[id]
	if !ok {
		return Peer{}, false
	}
	return e.peer, true
}

// peersMsgLocked is the "peers" message for the current peer list.
func (h *hub) peersMsgLocked() []byte {
	b, err := json.Marshal(struct {
		Type     string `json:"type"`
		Peers    []Peer `json:"peers"`
		Complete bool   `json:"complete"`
	}{"peers", h.peerListLocked(), h.browsed})
	if err != nil {
		panic(err) // only plain structs are encoded
	}
	return b
}

// peerListLocked is the peer list, this daemon first, then by name. The
// self entry always comes from Info: mDNS TXT records are unauthenticated
// and our server id is public on the LAN, so records claiming it are
// dropped rather than trusted to describe this daemon.
func (h *hub) peerListLocked() []Peer {
	info := h.src.Info()
	self := Peer{ID: info.ID, Name: info.Name, Host: info.Hostname, Addrs: []string{}, Self: true}
	if _, port, err := net.SplitHostPort(info.Listen); err == nil {
		self.Port, _ = strconv.Atoi(port)
	}
	out := []Peer{}
	for _, e := range h.peerSet {
		if info.ID != "" && e.peer.ID == info.ID {
			continue
		}
		out = append(out, e.peer)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID+out[i].Host < out[j].ID+out[j].Host
	})
	return append([]Peer{self}, out...)
}
