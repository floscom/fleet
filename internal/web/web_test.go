package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/discovery"
)

// fakeSource feeds the hub from a channel the test writes to.
type fakeSource struct {
	// id is the server id, default "self-id".
	id     string
	events chan *fleetv1.Event
	mu     sync.Mutex
	devs   []Device
	added  []*fleetv1.AddRootRequest
	// addErr and removeErr are returned by AddRoot and RemoveRoot.
	addErr, removeErr error
	removed           []string

	// Agents: runs, stops and inputs received; chat is what Chat returns
	// (Agent filled in from agent).
	agent  *fleetv1.Agent
	runs   []*fleetv1.RunAgentRequest
	stops  []*fleetv1.KillAgentRequest
	inputs []fakeInput
	// dialog makes SendInput report Enter held back, as for an open dialog.
	dialog bool
	chat   Chat
}

type fakeInput struct {
	agent, text string
	submit      bool
	keys        []string
}

func newFakeSource(agents ...*fleetv1.Agent) *fakeSource {
	s := &fakeSource{events: make(chan *fleetv1.Event, 64)}
	for _, a := range agents {
		s.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a}}
	}
	s.events <- &fleetv1.Event{Kind: &fleetv1.Event_SnapshotDone{SnapshotDone: &fleetv1.SnapshotDone{}}}
	return s
}

func (s *fakeSource) Info() Info {
	id := s.id
	if id == "" {
		id = "self-id"
	}
	return Info{ID: id, Name: "studio", Version: "v0.3.0", Hostname: "studio", OS: "linux", Arch: "amd64", Listen: "0.0.0.0:7420", MDNS: true}
}

func (s *fakeSource) SubscribeAgents() (<-chan *fleetv1.Event, func()) {
	return s.events, func() {}
}

func (s *fakeSource) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Device(nil), s.devs...)
}

func (s *fakeSource) Roots() []*fleetv1.Root {
	return []*fleetv1.Root{{Name: "code", Path: "/home/flo/code", Trust: true}}
}

func (s *fakeSource) AddRoot(req *fleetv1.AddRootRequest) (*fleetv1.Root, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return nil, s.addErr
	}
	s.added = append(s.added, req)
	return &fleetv1.Root{Name: req.Name, Path: req.Path, Adapters: req.Adapters, Trust: req.Trust}, nil
}

func (s *fakeSource) RemoveRoot(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, name)
	return s.removeErr
}

func (s *fakeSource) Adapters(context.Context) []Adapter {
	return []Adapter{{ID: "claude", Name: "Claude Code", Available: true}}
}

func (s *fakeSource) Agents() []*fleetv1.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil {
		return nil
	}
	return []*fleetv1.Agent{s.agent}
}

func (s *fakeSource) RunAgent(_ context.Context, req *fleetv1.RunAgentRequest) (*fleetv1.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Root != "code" {
		return nil, &Error{Status: 404, Msg: "unknown root " + req.Root}
	}
	s.runs = append(s.runs, req)
	return &fleetv1.Agent{Id: "a1", Name: "claude-api-1", Adapter: req.Adapter, State: fleetv1.AgentState_AGENT_STATE_STARTING}, nil
}

func (s *fakeSource) StopAgent(_ context.Context, req *fleetv1.KillAgentRequest) (*fleetv1.KillAgentResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, req)
	return &fleetv1.KillAgentResponse{Agent: &fleetv1.Agent{Id: req.Agent, State: fleetv1.AgentState_AGENT_STATE_EXITED}}, nil
}

func (s *fakeSource) SendInput(_ context.Context, agent, text string, submit bool, keys []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent != "a1" {
		return false, &Error{Status: 404, Msg: "no agent " + agent}
	}
	s.inputs = append(s.inputs, fakeInput{agent, text, submit, keys})
	return s.dialog && submit && text != "", nil
}

func (s *fakeSource) Screen(_ context.Context, agent string) (string, error) {
	if agent != "a1" {
		return "", &Error{Status: 404, Msg: "no agent " + agent}
	}
	return "> hello\n\n", nil
}

func (s *fakeSource) Chat(agent string) (Chat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil || agent != s.agent.Id {
		return Chat{}, &Error{Status: 404, Msg: "no agent " + agent}
	}
	c := s.chat
	c.Agent = s.agent
	return c, nil
}

// startServer runs a Server on an httptest listener until the test ends.
func startServer(t *testing.T, src Source) *httptest.Server {
	return startServerToken(t, src, "")
}

// startServerToken is startServer with the admin token file at tokenPath.
func startServerToken(t *testing.T, src Source, tokenPath string) *httptest.Server {
	t.Helper()
	return startServerOpts(t, Options{
		Source:    src,
		TokenPath: tokenPath,
		Browse: func(ctx context.Context, _ time.Duration) ([]discovery.Found, error) {
			return []discovery.Found{
				{ServerID: "self-id", Name: "studio", Host: "studio.local", Addrs: []string{"192.168.1.5"}, Port: 7420},
				{ServerID: "peer-id", Name: "attic", Host: "attic.local", Addrs: []string{"192.168.1.9"}, Port: 7420, WebPort: 7421},
			}, nil
		},
	})
}

// startServerOpts runs a Server with opts (Addr is filled in).
func startServerOpts(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.start(ctx)
	ts := httptest.NewServer(s.handler())
	t.Cleanup(func() {
		cancel()
		s.wg.Wait()
		ts.Close()
	})
	return ts
}

func TestCheckAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7421": true, "[::1]:7421": true, "localhost:7421": true, "0.0.0.0:7421": true, ":7421": true,
		"zerox:7421": true, "127.0.0.1": false, "": false,
	} {
		if err := CheckAddr(addr); (err == nil) != ok {
			t.Errorf("CheckAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestStaticAndHeaders(t *testing.T) {
	ts := startServer(t, newFakeSource())
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != csp {
		t.Fatalf("CSP = %q", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers: %v", resp.Header)
	}
	// The Tailwind source is not served under any spelling of its path
	// (sent raw, without client-side path cleaning).
	for _, path := range []string{"/input.css", "//input.css", "/./input.css", "/x/../input.css", "/%2e/input.css"} {
		if code := rawGet(t, ts, path); code != 404 {
			t.Fatalf("%s: %d, want 404", path, code)
		}
	}
	if code := rawGet(t, ts, "/app.css"); code != 200 {
		t.Fatalf("app.css: %d, want 200", code)
	}
}

// TestIcons checks that every icon the page and the manifest name is served
// as an image.
func TestIcons(t *testing.T) {
	ts := startServer(t, newFakeSource())
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
		}
		return resp, body
	}
	_, index := get("/")
	var paths []string
	for _, m := range regexp.MustCompile(`<link rel="(?:icon|apple-touch-icon)" href="([^"]+)"`).FindAllSubmatch(index, -1) {
		paths = append(paths, string(m[1]))
	}
	resp, body := get("/manifest.webmanifest")
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Fatalf("manifest Content-Type = %q", ct)
	}
	var manifest struct{ Icons []struct{ Src string } }
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, icon := range manifest.Icons {
		paths = append(paths, icon.Src)
	}
	if len(paths) < 6 {
		t.Fatalf("icons: %v", paths)
	}
	for _, p := range paths {
		if resp, _ := get(p); !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			t.Fatalf("%s: Content-Type %q", p, resp.Header.Get("Content-Type"))
		}
	}
}

// rawGet sends GET path verbatim and returns the status code.
func rawGet(t *testing.T, ts *httptest.Server, path string) int {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", path)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestForeignHostRejected(t *testing.T) {
	ts := startServer(t, newFakeSource())
	for _, path := range []string{"/", "/ws", "/api/session"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = "evil.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with Host evil.com: %d, want 403", path, resp.StatusCode)
		}
	}
}

func wsURL(ts *httptest.Server) string { return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws" }

func TestWebSocketForeignOriginRejected(t *testing.T) {
	ts := startServer(t, newFakeSource())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://evil.com"}},
	})
	if err == nil {
		c.CloseNow()
		t.Fatal("dial with a foreign Origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("resp = %v, want 403", resp)
	}
}

type msg struct {
	Type     string          `json:"type"`
	Server   *Info           `json:"server"`
	Agent    *Agent          `json:"agent"`
	ID       string          `json:"id"`
	Peers    []Peer          `json:"peers"`
	Complete bool            `json:"complete"`
	Devices  json.RawMessage `json:"devices"`
	Roots    []Root          `json:"roots"`
}

func TestWebSocketSnapshotAndLive(t *testing.T) {
	src := newFakeSource(
		&fleetv1.Agent{Id: "a1", Name: "api-fix", State: fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT,
			Isolation: fleetv1.Isolation_ISOLATION_WORKTREE, Sandbox: fleetv1.Sandbox_SANDBOX_DOCKER, CreatedAtMs: 1},
		&fleetv1.Agent{Id: "a2", Name: "done", State: fleetv1.AgentState_AGENT_STATE_EXITED, HasExitCode: true, CreatedAtMs: 2},
	)
	ts := startServer(t, src)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read := func() msg {
		t.Helper()
		var m msg
		if err := wsjson.Read(ctx, c, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	want := []string{"hello", "devices", "peers", "roots", "agent", "agent", "snapshotDone"}
	var got []msg
	for range want {
		got = append(got, read())
	}
	for i, m := range got {
		if m.Type != want[i] {
			t.Fatalf("message %d: %q, want %q", i, m.Type, want[i])
		}
	}
	if got[0].Server.ID != "self-id" || string(got[1].Devices) != "[]" {
		t.Fatalf("hello/devices: %+v %s", got[0].Server, got[1].Devices)
	}
	if len(got[2].Peers) != 1 || !got[2].Peers[0].Self || got[2].Peers[0].Port != 7420 || got[2].Complete {
		t.Fatalf("initial peers: %+v", got[2].Peers)
	}
	if len(got[3].Roots) != 1 || got[3].Roots[0].Adapters == nil {
		t.Fatalf("roots: %+v", got[3].Roots)
	}
	a1, a2 := got[4].Agent, got[5].Agent
	if a1.ID != "a1" || a1.State != "needs_input" || a1.Isolation != "worktree" || a1.Sandbox != "docker" || a1.ExitCode != nil {
		t.Fatalf("agent a1: %+v", a1)
	}
	if a2.State != "exited" || a2.ExitCode == nil || *a2.ExitCode != 0 {
		t.Fatalf("agent a2: %+v", a2)
	}

	// Live: an agent changes, one goes away, the peer browse finds another
	// daemon and a device appears (in any order relative to each other).
	src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: &fleetv1.Agent{
		Id: "a1", Name: "api-fix", State: fleetv1.AgentState_AGENT_STATE_WORKING, CreatedAtMs: 1}}}
	src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentRemoved{AgentRemoved: "a2"}}
	src.mu.Lock()
	src.devs = []Device{{ID: "d1", Name: "flo-mbp", Connected: true}}
	src.mu.Unlock()
	var working, removed, peers, devices bool
	for !(working && removed && peers && devices) {
		m := read()
		switch m.Type {
		case "agent":
			working = m.Agent.ID == "a1" && m.Agent.State == "working"
		case "agentRemoved":
			removed = m.ID == "a2"
		case "peers":
			// The mDNS record claiming our id does not replace the self
			// entry, which always comes from Info.
			peers = m.Complete && len(m.Peers) == 2 && m.Peers[0].Self && m.Peers[0].Host == "studio" &&
				len(m.Peers[0].Addrs) == 0 && m.Peers[1].ID == "peer-id" && m.Peers[1].WebPort == 7421
		case "devices":
			devices = strings.Contains(string(m.Devices), `"flo-mbp"`)
		default:
			t.Fatalf("unexpected %q", m.Type)
		}
	}
}

func TestEnumNames(t *testing.T) {
	a := agentOf(&fleetv1.Agent{State: fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT})
	if a.State != "needs_input" || a.Isolation != "unspecified" || a.Sandbox != "unspecified" {
		t.Fatalf("%+v", a)
	}
	b := encode("agent", "agent", a)
	if !strings.HasPrefix(string(b), `{"type":"agent","agent":{`) || !strings.Contains(string(b), `"exitCode":null`) {
		t.Fatalf("%s", b)
	}
}
