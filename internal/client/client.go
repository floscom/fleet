// Package client is the Go client for the fleet protocol. It is used by the
// fleet CLI and is a reference for native apps.
//
// A Client wraps one connection to a daemon. Requests are correlated by id,
// so helpers may be called concurrently; pushed messages (events, terminal
// output) are routed to the Subscribe channel and to attached Terminals.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/wire"
)

// handshakeTimeout bounds reading ServerHello when ctx has no deadline.
const handshakeTimeout = 10 * time.Second

// ErrClosed is returned by calls on a closed client.
var ErrClosed = errors.New("connection closed")

// Error is a daemon-reported failure (a ServerMessage.error response).
type Error struct {
	Code    fleetv1.ErrorCode
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(e.Code.String(), "ERROR_CODE_"), "_", " "))
}

// Code returns the ErrorCode carried by err, or ERROR_CODE_UNSPECIFIED if err
// is not (and does not wrap) an *Error.
func Code(err error) fleetv1.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fleetv1.ErrorCode_ERROR_CODE_UNSPECIFIED
}

// Client is one connection to a fleet daemon.
type Client struct {
	conn   *wire.Conn
	hello  *fleetv1.ServerHello
	nextID atomic.Uint64
	done   chan struct{}

	mu      sync.Mutex
	err     error // why the connection ended; set before done is closed
	pending map[uint64]*pending
	subs    map[*stream[*fleetv1.Event]]struct{}
	terms   map[string]*Terminal
}

type pending struct {
	ch chan *fleetv1.ServerMessage
	// pre runs on the reader goroutine before the response is delivered, so
	// pushed messages that follow the response cannot be missed.
	pre func(*fleetv1.ServerMessage)
	// late, if set, runs on the reader goroutine instead of pre when the
	// response arrives after the caller gave up (it must not block), so the
	// caller can undo what the request did on the daemon.
	late func(*fleetv1.ServerMessage)
	// abandoned is set (under Client.mu) when the caller gave up and late
	// is to handle the response.
	abandoned bool
}

// handshake reads ServerHello from rw, honouring ctx, and checks the
// protocol version.
func handshake(ctx context.Context, rw net.Conn) (*wire.Conn, *fleetv1.ServerHello, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handshakeTimeout)
	}
	_ = rw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = rw.SetDeadline(time.Now()) })
	conn := wire.NewConn(rw)
	m, err := conn.RecvServer()
	if !stop() && ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read server hello: %w", err)
	}
	_ = rw.SetDeadline(time.Time{})
	if e := m.GetError(); e != nil {
		return nil, nil, &Error{Code: e.Code, Message: e.Message}
	}
	hello := m.GetHello()
	if hello == nil {
		return nil, nil, errors.New("server did not send a hello")
	}
	if hello.ProtocolVersion != fleetv1.ProtocolVersion_PROTOCOL_VERSION_1 {
		return nil, nil, &Error{
			Code:    fleetv1.ErrorCode_ERROR_CODE_UNSUPPORTED_PROTOCOL,
			Message: fmt.Sprintf("unsupported daemon protocol version %d", hello.ProtocolVersion),
		}
	}
	return conn, hello, nil
}

// newClient starts the reader goroutine on an established connection.
func newClient(conn *wire.Conn, hello *fleetv1.ServerHello) *Client {
	c := &Client{
		conn:    conn,
		hello:   hello,
		done:    make(chan struct{}),
		pending: map[uint64]*pending{},
		subs:    map[*stream[*fleetv1.Event]]struct{}{},
		terms:   map[string]*Terminal{},
	}
	go c.readLoop()
	return c
}

// Hello returns the daemon's ServerHello.
func (c *Client) Hello() *fleetv1.ServerHello { return c.hello }

// ServerID returns the daemon's id (hex SHA-256 of its certificate).
func (c *Client) ServerID() string { return c.hello.GetServerId() }

// ServerName returns the daemon's display name.
func (c *Client) ServerName() string { return c.hello.GetServerName() }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (nil while it is open).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close closes the connection. Pending calls fail with ErrClosed, event
// channels and terminals are closed. It returns even if a consumer stopped
// reading its event channel or terminal output.
func (c *Client) Close() error {
	err := c.conn.Close()
	// The reader may be blocked delivering to a full stream; closing the
	// streams releases it so it can see the closed connection.
	c.mu.Lock()
	subs := make([]*stream[*fleetv1.Event], 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	terms := make([]*Terminal, 0, len(c.terms))
	for _, t := range c.terms {
		terms = append(terms, t)
	}
	c.mu.Unlock()
	for _, s := range subs {
		s.close()
	}
	for _, t := range terms {
		t.finish("connection closed")
	}
	<-c.done
	return err
}

func (c *Client) readLoop() {
	var err error
	for {
		var m *fleetv1.ServerMessage
		if m, err = c.conn.RecvServer(); err != nil {
			break
		}
		c.dispatch(m)
	}
	c.shutdown(err)
}

func (c *Client) dispatch(m *fleetv1.ServerMessage) {
	if m.Id != 0 {
		c.mu.Lock()
		p := c.pending[m.Id]
		delete(c.pending, m.Id)
		abandoned := p != nil && p.abandoned
		c.mu.Unlock()
		if p == nil {
			return // caller gave up (context cancelled)
		}
		if abandoned {
			p.late(m)
			return
		}
		if p.pre != nil {
			p.pre(m)
		}
		p.ch <- m
		return
	}
	switch msg := m.Msg.(type) {
	case *fleetv1.ServerMessage_Event:
		c.mu.Lock()
		subs := make([]*stream[*fleetv1.Event], 0, len(c.subs))
		for s := range c.subs {
			subs = append(subs, s)
		}
		c.mu.Unlock()
		for _, s := range subs {
			s.send(msg.Event)
		}
	case *fleetv1.ServerMessage_TerminalOutput:
		if t := c.terminal(msg.TerminalOutput.AgentId); t != nil {
			t.out.send(msg.TerminalOutput.Data)
		}
	case *fleetv1.ServerMessage_TerminalClosed:
		if t := c.terminal(msg.TerminalClosed.AgentId); t != nil {
			t.finish(msg.TerminalClosed.Reason)
		}
	}
}

func (c *Client) terminal(agentID string) *Terminal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terms[agentID]
}

func (c *Client) shutdown(cause error) {
	c.mu.Lock()
	if cause == nil || errors.Is(cause, net.ErrClosed) {
		c.err = ErrClosed
	} else {
		c.err = fmt.Errorf("%w: %v", ErrClosed, cause)
	}
	subs, terms := c.subs, c.terms
	c.subs, c.terms, c.pending = map[*stream[*fleetv1.Event]]struct{}{}, map[string]*Terminal{}, map[uint64]*pending{}
	close(c.done)
	c.mu.Unlock()
	_ = c.conn.Close()
	for s := range subs {
		s.close()
	}
	for _, t := range terms {
		t.finish("connection closed")
	}
}

// call sends a request and waits for its response. Error responses become
// *Error. pre and late are optional; see pending.
func (c *Client) call(ctx context.Context, m *fleetv1.ClientMessage, pre, late func(*fleetv1.ServerMessage)) (*fleetv1.ServerMessage, error) {
	id := c.nextID.Add(1)
	m.Id = id
	p := &pending{ch: make(chan *fleetv1.ServerMessage, 1), pre: pre, late: late}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = p
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.conn.SendClient(m); err != nil {
		forget()
		return nil, fmt.Errorf("send: %w", err)
	}
	var r *fleetv1.ServerMessage
	select {
	case r = <-p.ch:
	case <-ctx.Done():
		c.mu.Lock()
		if late != nil {
			p.abandoned = true // no-op if the response is being delivered
		} else {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		select {
		case r = <-p.ch:
		default:
			return nil, c.Err()
		}
	}
	if e := r.GetError(); e != nil {
		return nil, &Error{Code: e.Code, Message: e.Message}
	}
	return r, nil
}

// unary performs call and extracts the typed response with get.
func unary[T interface {
	comparable
	fmt.Stringer
}](ctx context.Context, c *Client, req *fleetv1.ClientMessage, get func(*fleetv1.ServerMessage) T) (T, error) {
	var zero T
	m, err := c.call(ctx, req, nil, nil)
	if err != nil {
		return zero, err
	}
	r := get(m)
	if r == zero {
		return zero, fmt.Errorf("unexpected response %T", m.Msg)
	}
	return r, nil
}

// stream is a channel that a single producer (the reader goroutine) feeds
// and that can be closed from any goroutine without racing a send.
type stream[T any] struct {
	ch     chan T
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	closed bool
}

func newStream[T any](size int) *stream[T] {
	return &stream[T]{ch: make(chan T, size), stop: make(chan struct{})}
}

// send delivers v, blocking while the consumer is behind (back-pressure),
// until the stream is closed.
func (s *stream[T]) send(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- v:
	case <-s.stop:
	}
}

func (s *stream[T]) close() {
	s.once.Do(func() {
		close(s.stop) // unblocks a pending send so we can take the lock
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	})
}
