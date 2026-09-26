package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/identity"
	"fleet/internal/wire"
)

// handler answers one client message on a fake daemon connection. It may
// send any number of messages (responses and pushes).
type handler func(t *testing.T, conn *wire.Conn, hello *fleetv1.ServerHello, m *fleetv1.ClientMessage)

type fakeServer struct {
	t     *testing.T
	ln    net.Listener
	hello func() *fleetv1.ServerHello
	h     handler
	wg    sync.WaitGroup
}

func (s *fakeServer) serve() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			conn := wire.NewConn(nc)
			defer conn.Close()
			hello := s.hello()
			if tc, ok := nc.(*tls.Conn); ok {
				if err := tc.Handshake(); err != nil {
					return
				}
			}
			if err := conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Hello{Hello: hello}}); err != nil {
				return
			}
			for {
				m, err := conn.RecvClient()
				if err != nil {
					return
				}
				s.h(s.t, conn, hello, m)
			}
		}()
	}
}

func startFake(t *testing.T, ln net.Listener, hello func() *fleetv1.ServerHello, h handler) *fakeServer {
	s := &fakeServer{t: t, ln: ln, hello: hello, h: h}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func v1Hello(auth bool) func() *fleetv1.ServerHello {
	return func() *fleetv1.ServerHello {
		nonce := make([]byte, 32)
		rand.Read(nonce)
		return &fleetv1.ServerHello{ProtocolVersion: fleetv1.ProtocolVersion_PROTOCOL_VERSION_1, ServerName: "test", Nonce: nonce, AuthRequired: auth}
	}
}

func reply(conn *wire.Conn, id uint64, m *fleetv1.ServerMessage) {
	m.Id = id
	_ = conn.SendServer(m)
}

// localFake starts a fake daemon on FLEET_HOME's socket.
func localFake(t *testing.T, hello func() *fleetv1.ServerHello, h handler) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := config.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", config.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	startFake(t, ln, hello, h)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestDialLocalNotRunning(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := DialLocal(ctxT(t)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

func TestDialLocalRejectsBadHello(t *testing.T) {
	tests := []struct {
		name  string
		hello *fleetv1.ServerHello
	}{
		{"auth required", &fleetv1.ServerHello{ProtocolVersion: fleetv1.ProtocolVersion_PROTOCOL_VERSION_1, AuthRequired: true}},
		{"wrong version", &fleetv1.ServerHello{ProtocolVersion: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localFake(t, func() *fleetv1.ServerHello { return tt.hello }, func(*testing.T, *wire.Conn, *fleetv1.ServerHello, *fleetv1.ClientMessage) {})
			if c, err := DialLocal(ctxT(t)); err == nil {
				c.Close()
				t.Fatal("DialLocal succeeded")
			}
		})
	}
}

func TestCorrelationAndErrors(t *testing.T) {
	// The fake holds SendText requests and answers each batch of 4 in reverse
	// order, echoing the agent name in an error.
	var mu sync.Mutex
	var held []*fleetv1.ClientMessage
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		if m.GetSendText() == nil {
			// Wrong response type on purpose.
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_GetInfo{GetInfo: &fleetv1.GetInfoResponse{}}})
			return
		}
		mu.Lock()
		held = append(held, m)
		if len(held) < 4 {
			mu.Unlock()
			return
		}
		batch := held
		held = nil
		mu.Unlock()
		for i := len(batch) - 1; i >= 0; i-- {
			r := batch[i]
			reply(conn, r.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Error{Error: &fleetv1.Error{
				Code: fleetv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: r.GetSendText().Agent}}})
		}
	})
	ctx := ctxT(t)
	c, err := DialLocal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(agent string) {
			defer wg.Done()
			err := c.SendText(ctx, agent, "hi", true)
			var e *Error
			if !errors.As(err, &e) || e.Code != fleetv1.ErrorCode_ERROR_CODE_NOT_FOUND || e.Message != agent {
				t.Errorf("SendText(%s) err = %v, want not found %q", agent, err, agent)
			}
		}(fmt.Sprintf("agent-%d", i))
	}
	wg.Wait()

	if _, err := c.ListRoots(ctx); err == nil {
		t.Fatal("mismatched response type accepted")
	}
	if Code(errors.New("x")) != fleetv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatal("Code of plain error")
	}
}

func TestCloseFailsPending(t *testing.T) {
	localFake(t, v1Hello(false), func(*testing.T, *wire.Conn, *fleetv1.ServerHello, *fleetv1.ClientMessage) {}) // never answers
	ctx := ctxT(t)
	c, err := DialLocal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := c.GetInfo(ctx); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if _, err := c.GetInfo(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close err = %v", err)
	}
}

func TestSubscribe(t *testing.T) {
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		if m.GetSubscribe() == nil {
			return
		}
		reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Subscribe{Subscribe: &fleetv1.SubscribeResponse{}}})
		push := func(e *fleetv1.Event) {
			_ = conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Event{Event: e}})
		}
		push(&fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: &fleetv1.Agent{Id: "a1"}}})
		push(&fleetv1.Event{Kind: &fleetv1.Event_SnapshotDone{SnapshotDone: &fleetv1.SnapshotDone{}}})
		push(&fleetv1.Event{Kind: &fleetv1.Event_AgentRemoved{AgentRemoved: "a1"}})
	})
	c, err := DialLocal(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(ctxT(t))
	ch, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := <-ch; e.GetAgentUpserted().GetId() != "a1" {
		t.Fatalf("first event %v", e)
	}
	if e := <-ch; e.GetSnapshotDone() == nil {
		t.Fatalf("second event %v", e)
	}
	if e := <-ch; e.GetAgentRemoved() != "a1" {
		t.Fatalf("third event %v", e)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected event after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestAttach(t *testing.T) {
	got := make(chan *fleetv1.ClientMessage, 10)
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		switch msg := m.Msg.(type) {
		case *fleetv1.ClientMessage_Attach:
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Attach{Attach: &fleetv1.AttachResponse{AgentId: "abc123"}}})
			// Output pushed right behind the response must not be lost.
			_ = conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_TerminalOutput{TerminalOutput: &fleetv1.TerminalOutput{AgentId: "abc123", Data: []byte("hello")}}})
			_ = conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_TerminalOutput{TerminalOutput: &fleetv1.TerminalOutput{AgentId: "other", Data: []byte("nope")}}})
			if msg.Attach.Agent != "my-agent" || msg.Attach.Cols != 80 {
				t.Errorf("attach request %v", msg.Attach)
			}
		case *fleetv1.ClientMessage_TerminalInput, *fleetv1.ClientMessage_TerminalResize:
			got <- m
			if in := m.GetTerminalInput(); in != nil && string(in.Data) == "exit\r" {
				_ = conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_TerminalClosed{TerminalClosed: &fleetv1.TerminalClosed{AgentId: "abc123", Reason: "agent exited"}}})
			}
		}
	})
	ctx := ctxT(t)
	c, err := DialLocal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	term, err := c.Attach(ctx, "my-agent", AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if term.AgentID() != "abc123" {
		t.Fatalf("AgentID = %q", term.AgentID())
	}
	if b := <-term.Output(); string(b) != "hello" {
		t.Fatalf("output %q", b)
	}
	if err := term.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	if m := <-got; m.GetTerminalResize().GetCols() != 100 || m.GetTerminalResize().GetAgentId() != "abc123" || m.Id != 0 {
		t.Fatalf("resize %v", m)
	}
	if _, err := term.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	<-got
	select {
	case <-term.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("terminal not closed")
	}
	if term.Reason() != "agent exited" {
		t.Fatalf("reason %q", term.Reason())
	}
	for b := range term.Output() {
		t.Fatalf("unexpected output %q", b) // "nope" was for another agent
	}
}

func TestAttachReadOnly(t *testing.T) {
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		if m.GetAttach() != nil {
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Attach{Attach: &fleetv1.AttachResponse{AgentId: "x"}}})
		}
	})
	c, err := DialLocal(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	term, err := c.Attach(ctxT(t), "x", AttachOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v", err)
	}
	c.Close()
	if term.Reason() != "connection closed" {
		t.Fatalf("reason %q", term.Reason())
	}
}

// remoteFake runs a TLS fake daemon implementing auth and pairing with the
// real identity primitives. serverProof may tamper with the pairing proof.
func remoteFake(t *testing.T, code string, tamper bool) (addr string, srv *identity.Server, devs *sync.Map) {
	srv, err := identity.LoadOrCreateServer(t.TempDir(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	devs = &sync.Map{} // device id -> public key
	var mu sync.Mutex
	authed := map[*wire.Conn]bool{}
	startFake(t, ln, func() *fleetv1.ServerHello {
		h := v1Hello(true)()
		h.ServerId = srv.ID
		return h
	}, func(t *testing.T, conn *wire.Conn, hello *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		nonce := hello.Nonce
		fail := func(code fleetv1.ErrorCode) {
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Error{Error: &fleetv1.Error{Code: code, Message: code.String()}}})
		}
		switch msg := m.Msg.(type) {
		case *fleetv1.ClientMessage_Pair:
			p := msg.Pair
			if string(p.Proof) != string(identity.PairProof(code, srv.Fingerprint, p.DevicePublicKey)) {
				fail(fleetv1.ErrorCode_ERROR_CODE_PAIRING_FAILED)
				return
			}
			id := identity.DeviceID(p.DevicePublicKey)
			devs.Store(id, p.DevicePublicKey)
			proof := identity.ServerPairProof(code, srv.Fingerprint, p.DevicePublicKey)
			if tamper {
				proof[0] ^= 1
			}
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Pair{Pair: &fleetv1.PairResponse{
				DeviceId: id, ServerProof: proof, ServerId: srv.ID, ServerName: "fake"}}})
		case *fleetv1.ClientMessage_Auth:
			pub, ok := devs.Load(msg.Auth.DeviceId)
			if !ok || !identity.VerifyAuth(pub.([]byte), nonce, srv.Fingerprint, msg.Auth.Signature) {
				fail(fleetv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
				return
			}
			mu.Lock()
			authed[conn] = true
			mu.Unlock()
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Auth{Auth: &fleetv1.AuthResponse{DeviceName: "dev"}}})
		case *fleetv1.ClientMessage_Ping:
			mu.Lock()
			ok := authed[conn]
			mu.Unlock()
			if !ok {
				fail(fleetv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
				return
			}
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Pong{Pong: &fleetv1.Pong{}}})
		}
	})
	return ln.Addr().String(), srv, devs
}

func newDevice(t *testing.T) *identity.Device {
	d, err := identity.LoadOrCreateDevice(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPairThenDialRemote(t *testing.T) {
	const code = "ABCD-EFGH"
	addr, srv, devs := remoteFake(t, code, false)
	dev := newDevice(t)
	ctx := ctxT(t)

	if _, err := Pair(ctx, addr, "ABCD-EFGX", "laptop", dev); Code(err) != fleetv1.ErrorCode_ERROR_CODE_PAIRING_FAILED {
		t.Fatalf("wrong code: err = %v", err)
	}
	if _, err := Pair(ctx, addr, "nope", "laptop", dev); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("malformed code: err = %v", err)
	}
	known, err := Pair(ctx, addr, "abcd efgh", "laptop", dev)
	if err != nil {
		t.Fatal(err)
	}
	if known.ID != srv.ID || known.Name != "fake" || known.Address != addr || known.DeviceID != dev.ID {
		t.Fatalf("known = %+v", known)
	}
	if _, ok := devs.Load(dev.ID); !ok {
		t.Fatal("device not registered")
	}

	c, err := DialRemote(ctx, addr, known, dev)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// An unpaired device fails authentication.
	if _, err := DialRemote(ctx, addr, identity.KnownServer{ID: srv.ID}, newDevice(t)); Code(err) != fleetv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED {
		t.Fatalf("stranger: err = %v", err)
	}
}

func TestDialRemoteRejectsOtherCert(t *testing.T) {
	addr, _, _ := remoteFake(t, "ABCD-EFGH", false)
	other, err := identity.LoadOrCreateServer(t.TempDir(), "other")
	if err != nil {
		t.Fatal(err)
	}
	_, err = DialRemote(ctxT(t), addr, identity.KnownServer{ID: other.ID, Name: "other"}, newDevice(t))
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
}

func TestPairRejectsBadServerProof(t *testing.T) {
	addr, _, _ := remoteFake(t, "ABCD-EFGH", true)
	_, err := Pair(ctxT(t), addr, "ABCD-EFGH", "laptop", newDevice(t))
	if !errors.Is(err, ErrServerProof) {
		t.Fatalf("err = %v, want ErrServerProof", err)
	}
}

func TestConnectKnownServer(t *testing.T) {
	const code = "ABCD-EFGH"
	addr, _, _ := remoteFake(t, code, false)
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	dev, err := LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)
	known, err := Pair(ctx, addr, code, "laptop", dev)
	if err != nil {
		t.Fatal(err)
	}
	store, err := identity.OpenServerStore(filepath.Join(home, "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(known); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"fake", known.ID[:8], addr} {
		c, err := Connect(ctx, target)
		if err != nil {
			t.Fatalf("Connect(%q): %v", target, err)
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if _, err := Connect(ctx, "unknown-box"); err == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestHostPort(t *testing.T) {
	tests := map[string]string{
		"box":            "box:7420",
		"box:1":          "box:1",
		"10.0.0.2":       "10.0.0.2:7420",
		"::1":            "[::1]:7420",
		"[::1]":          "[::1]:7420",
		"[fe80::1]:9000": "[fe80::1]:9000",
	}
	for in, want := range tests {
		if got := HostPort(in); got != want {
			t.Errorf("HostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

// Close must return even when the reader is blocked on a subscriber that
// stopped reading.
func TestCloseWithStalledSubscriber(t *testing.T) {
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		if m.GetSubscribe() == nil {
			return
		}
		reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Subscribe{Subscribe: &fleetv1.SubscribeResponse{}}})
		for i := 0; i < 1000; i++ {
			if conn.SendServer(&fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Event{Event: &fleetv1.Event{
				Kind: &fleetv1.Event_AgentRemoved{AgentRemoved: fmt.Sprint(i)},
			}}}) != nil {
				return
			}
		}
	})
	c, err := DialLocal(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-ch // read one, then stop reading
	time.Sleep(100 * time.Millisecond)
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a stalled subscriber")
	}
}

// An Attach whose response arrives after ctx was cancelled is detached.
func TestAttachCancelledDetachesLate(t *testing.T) {
	detached := make(chan string, 1)
	localFake(t, v1Hello(false), func(t *testing.T, conn *wire.Conn, _ *fleetv1.ServerHello, m *fleetv1.ClientMessage) {
		switch {
		case m.GetAttach() != nil:
			time.Sleep(200 * time.Millisecond)
			reply(conn, m.Id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Attach{Attach: &fleetv1.AttachResponse{AgentId: "a1"}}})
		case m.GetDetach() != nil:
			detached <- m.GetDetach().GetAgentId()
		}
	})
	c, err := DialLocal(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Attach(ctx, "a1", AttachOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Attach err = %v", err)
	}
	select {
	case id := <-detached:
		if id != "a1" {
			t.Fatalf("detached %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late attach was not detached")
	}
}

func TestPairVerifyFingerprint(t *testing.T) {
	const code = "ABCD-EFGH"
	addr, srv, devs := remoteFake(t, code, false)
	dev := newDevice(t)
	ctx := ctxT(t)

	if _, err := MatchFingerprint("abcd"); err == nil {
		t.Fatal("short fingerprint accepted")
	}
	wrong, err := MatchFingerprint(strings.Repeat("0", MinFingerprintLen))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PairVerify(ctx, addr, code, "laptop", dev, wrong); !errors.Is(err, ErrFingerprintRejected) {
		t.Fatalf("wrong fingerprint: err = %v", err)
	}
	if _, ok := devs.Load(dev.ID); ok {
		t.Fatal("proof was sent to an unconfirmed server")
	}
	// Grouped and upper-case, as `fleet pair` prints it.
	shown := strings.ToUpper(srv.ID[:4] + " " + srv.ID[4:8] + " " + srv.ID[8:20])
	right, err := MatchFingerprint(shown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PairVerify(ctx, addr, code, "laptop", dev, right); err != nil {
		t.Fatal(err)
	}
}
