package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/tmux"
	"fleet/internal/wire"
)

// testAdapter runs `sh -c <extra args joined>` (default: sleep 60).
type testAdapter struct{}

func (testAdapter) ID() string          { return "test" }
func (testAdapter) DisplayName() string { return "Test" }
func (testAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{ActivityState: true}
}
func (testAdapter) Detect(context.Context) adapter.Detection {
	return adapter.Detection{Available: true, Path: "/bin/sh"}
}

func (testAdapter) Launch(_ context.Context, req adapter.LaunchRequest) (*adapter.LaunchSpec, error) {
	script := strings.Join(req.ExtraArgs, " ")
	if script == "" {
		script = "sleep 60"
	}
	return &adapter.LaunchSpec{Argv: []string{"/bin/sh", "-c", script}, SessionID: "sess-" + req.AgentID}, nil
}

func (testAdapter) HandleHook(ev adapter.HookEvent) (adapter.StateUpdate, bool) {
	switch ev.Event {
	case "working":
		return adapter.StateUpdate{State: fleetv1.AgentState_AGENT_STATE_WORKING, Detail: string(ev.Payload)}, true
	case "exited": // hooks must never end an agent
		return adapter.StateUpdate{State: fleetv1.AgentState_AGENT_STATE_EXITED}, true
	}
	return adapter.StateUpdate{}, false
}

// unavailableAdapter is never installed.
type unavailableAdapter struct{ testAdapter }

func (unavailableAdapter) ID() string { return "missing" }
func (unavailableAdapter) Detect(context.Context) adapter.Detection {
	return adapter.Detection{Reason: "not installed"}
}

// env is a daemon running on a temp FLEET_HOME and a private tmux socket.
type env struct {
	t      *testing.T
	home   string
	root   string // real path of the "code" root
	tm     *tmux.Tmux
	listen string
	cancel context.CancelFunc
	done   chan error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	e := &env{t: t, home: home, root: root, tm: tmux.New("fleet-test-" + hex.EncodeToString(b))}
	cfg := fmt.Sprintf("name = \"test\"\nmdns = false\ntmux_socket = %q\n\n[[root]]\nname = \"code\"\npath = %q\n", e.tm.Socket, root)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.listen = ln.Addr().String()
	ln.Close()
	t.Cleanup(func() {
		e.stop()
		_ = exec.Command("tmux", "-L", e.tm.Socket, "kill-server").Run()
		dir := os.Getenv("TMUX_TMPDIR")
		if dir == "" {
			dir = "/tmp"
		}
		_ = os.Remove(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), e.tm.Socket))
	})
	e.start()
	return e
}

func (e *env) start() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan error, 1)
	opts := Options{
		Adapters:    adapter.NewRegistry(testAdapter{}, unavailableAdapter{}),
		Version:     "test",
		FleetBinary: "/bin/true",
		Listen:      e.listen,
		NoMDNS:      true,
	}
	go func() { e.done <- Run(ctx, opts) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-e.done:
			e.cancel = nil
			e.t.Fatalf("daemon exited: %v", err)
		default:
		}
		if c, err := net.Dial("unix", config.SocketPath()); err == nil {
			c.Close()
			if c, err := net.Dial("tcp", e.listen); err == nil {
				c.Close()
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("daemon did not start")
}

func (e *env) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.cancel = nil
	select {
	case err := <-e.done:
		if err != nil {
			e.t.Errorf("daemon: %v", err)
		}
	case <-time.After(10 * time.Second):
		e.t.Error("daemon did not stop")
	}
}

// sessions lists the fleet-* sessions on the test socket.
func (e *env) sessions() []string {
	e.t.Helper()
	panes, err := e.tm.List(context.Background())
	if err != nil {
		e.t.Fatalf("tmux list: %v", err)
	}
	var out []string
	for _, p := range panes {
		out = append(out, p.Session)
	}
	return out
}

func (e *env) mkdir(rel string) string {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// client is a test protocol client.
type client struct {
	t       *testing.T
	wc      *wire.Conn
	hello   *fleetv1.ServerHello
	fp      []byte // TLS only
	msgs    chan *fleetv1.ServerMessage
	backlog []*fleetv1.ServerMessage
	id      uint64
}

func newClient(t *testing.T, nc net.Conn) *client {
	t.Helper()
	c := &client{t: t, wc: wire.NewConn(nc), msgs: make(chan *fleetv1.ServerMessage, 4096)}
	t.Cleanup(func() { c.wc.Close() })
	go func() {
		defer close(c.msgs)
		for {
			m, err := c.wc.RecvServer()
			if err != nil {
				return
			}
			c.msgs <- m
		}
	}()
	m := c.next(5 * time.Second)
	if m == nil || m.GetHello() == nil {
		t.Fatalf("expected ServerHello, got %v", m)
	}
	c.hello = m.GetHello()
	return c
}

func (e *env) dialUnix() *client {
	e.t.Helper()
	nc, err := net.Dial("unix", config.SocketPath())
	if err != nil {
		e.t.Fatal(err)
	}
	return newClient(e.t, nc)
}

func (e *env) dialTLS() *client {
	e.t.Helper()
	tc, err := tls.Dial("tcp", e.listen, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(tc.ConnectionState().PeerCertificates[0].Raw)
	c := newClient(e.t, tc)
	c.fp = sum[:]
	return c
}

// next returns the next message (backlog first), or nil on close/timeout.
func (c *client) next(timeout time.Duration) *fleetv1.ServerMessage {
	if len(c.backlog) > 0 {
		m := c.backlog[0]
		c.backlog = c.backlog[1:]
		return m
	}
	select {
	case m := <-c.msgs:
		return m
	case <-time.After(timeout):
		return nil
	}
}

// closed reports whether the server closed the connection within timeout,
// discarding pending messages.
func (c *client) closed(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// call sends a request and waits for its response; pushed messages are kept.
func (c *client) call(m *fleetv1.ClientMessage) *fleetv1.ServerMessage {
	c.t.Helper()
	c.id++
	m.Id = c.id
	if err := c.wc.SendClient(m); err != nil {
		c.t.Fatalf("send: %v", err)
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case r, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("connection closed waiting for response to %T", m.GetMsg())
			}
			if r.GetId() == c.id {
				return r
			}
			c.backlog = append(c.backlog, r)
		case <-deadline:
			c.t.Fatalf("timeout waiting for response to %T", m.GetMsg())
		}
	}
}

// push sends a fire-and-forget message (id 0).
func (c *client) push(m *fleetv1.ClientMessage) {
	c.t.Helper()
	if err := c.wc.SendClient(m); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// ok calls and fails the test on an Error response.
func (c *client) ok(m *fleetv1.ClientMessage) *fleetv1.ServerMessage {
	c.t.Helper()
	r := c.call(m)
	if e := r.GetError(); e != nil {
		c.t.Fatalf("%T failed: %v %s", m.GetMsg(), e.GetCode(), e.GetMessage())
	}
	return r
}

// fails calls and asserts an Error with code.
func (c *client) fails(m *fleetv1.ClientMessage, code fleetv1.ErrorCode) *fleetv1.Error {
	c.t.Helper()
	r := c.call(m)
	e := r.GetError()
	if e == nil || e.GetCode() != code {
		c.t.Fatalf("%T: want error %v, got %v", m.GetMsg(), code, r)
	}
	return e
}

func runReq(r *fleetv1.RunAgentRequest) *fleetv1.ClientMessage {
	if r.Adapter == "" {
		r.Adapter = "test"
	}
	return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RunAgent{RunAgent: r}}
}

func killReq(r *fleetv1.KillAgentRequest) *fleetv1.ClientMessage {
	return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_KillAgent{KillAgent: r}}
}

func listReq(all bool) *fleetv1.ClientMessage {
	return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListAgents{ListAgents: &fleetv1.ListAgentsRequest{IncludeFinished: all}}}
}

func (c *client) run(r *fleetv1.RunAgentRequest) *fleetv1.Agent {
	c.t.Helper()
	return c.ok(runReq(r)).GetRunAgent().GetAgent()
}

// agent returns the agent with id from ListAgents(include_finished), or nil.
func (c *client) agent(id string) *fleetv1.Agent {
	c.t.Helper()
	for _, a := range c.ok(listReq(true)).GetListAgents().GetAgents() {
		if a.GetId() == id {
			return a
		}
	}
	return nil
}

// waitAgent polls until cond holds for the agent.
func (c *client) waitAgent(id string, what string, cond func(*fleetv1.Agent) bool) *fleetv1.Agent {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		a := c.agent(id)
		if a != nil && cond(a) {
			return a
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for agent %s to be %s; last: %v", id, what, a)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func finished(a *fleetv1.Agent) bool { return !isLive(a.GetState()) }

// waitFor polls cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
