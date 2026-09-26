package client

import (
	"context"
	"errors"
	"sync"

	fleetv1 "fleet/gen/fleetv1"
)

// ErrReadOnly is returned by Terminal.Write on a read-only attachment.
var ErrReadOnly = errors.New("terminal attached read-only")

// maxInputChunk bounds a single TerminalInput frame.
const maxInputChunk = 32 << 10

// AttachOptions configures Attach.
type AttachOptions struct {
	// Initial terminal size; 0 lets the daemon choose.
	Cols, Rows uint16
	// ReadOnly attaches without sending input.
	ReadOnly bool
}

// Terminal is a live view of an agent's terminal.
type Terminal struct {
	c        *Client
	agentID  string
	readOnly bool
	out      *stream[[]byte]
	done     chan struct{}
	once     sync.Once
	reason   string
}

// Attach streams an agent's terminal (id or name). Output is raw terminal
// bytes. Tip: use a dedicated Client per terminal so a busy terminal never
// delays control calls.
func (c *Client) Attach(ctx context.Context, agent string, opts AttachOptions) (*Terminal, error) {
	t := &Terminal{c: c, readOnly: opts.ReadOnly, out: newStream[[]byte](256), done: make(chan struct{})}
	var registered bool // guarded by c.mu
	register := func(m *fleetv1.ServerMessage) {
		r := m.GetAttach()
		if r == nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return
		}
		t.agentID = r.AgentId
		if old := c.terms[r.AgentId]; old != nil {
			go old.finish("attached again on this connection")
		}
		c.terms[r.AgentId] = t
		registered = true
	}
	// A response that arrives after ctx was cancelled still attached us on
	// the daemon: detach again so the connection does not keep streaming.
	late := func(m *fleetv1.ServerMessage) {
		if r := m.GetAttach(); r != nil {
			go c.detach(r.AgentId)
		}
	}
	_, err := c.call(ctx, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Attach{Attach: &fleetv1.AttachRequest{
		Agent: agent, Cols: uint32(opts.Cols), Rows: uint32(opts.Rows), ReadOnly: opts.ReadOnly,
	}}}, register, late)
	c.mu.Lock()
	reg := registered
	c.mu.Unlock()
	if err == nil && !reg {
		err = errors.New("unexpected attach response")
	}
	if err != nil {
		if reg { // response raced a cancelled ctx
			t.finish("attach cancelled")
			c.detach(t.agentID)
		}
		return nil, err
	}
	return t, nil
}

// detach sends a fire-and-forget DetachRequest (id 0: no reply is awaited).
func (c *Client) detach(agentID string) {
	_ = c.conn.SendClient(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Detach{Detach: &fleetv1.DetachRequest{AgentId: agentID}}})
}

// AgentID is the canonical id of the attached agent.
func (t *Terminal) AgentID() string { return t.agentID }

// Output delivers terminal output. It is closed when the terminal ends.
// Keep reading it: a full channel stalls the connection.
func (t *Terminal) Output() <-chan []byte { return t.out.ch }

// Done is closed when the terminal ends (agent exited, detached, or the
// connection closed).
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Reason says why the terminal ended; valid after Done is closed.
func (t *Terminal) Reason() string {
	<-t.done
	return t.reason
}

// Write sends keystrokes to the agent.
func (t *Terminal) Write(p []byte) (int, error) {
	if t.readOnly {
		return 0, ErrReadOnly
	}
	n := 0
	for len(p) > 0 {
		select {
		case <-t.done:
			return n, ErrClosed
		default:
		}
		chunk := p[:min(len(p), maxInputChunk)]
		err := t.c.conn.SendClient(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalInput{
			TerminalInput: &fleetv1.TerminalInput{AgentId: t.agentID, Data: chunk},
		}})
		if err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// Resize tells the daemon the viewer's terminal size.
func (t *Terminal) Resize(cols, rows uint16) error {
	select {
	case <-t.done:
		return ErrClosed
	default:
	}
	return t.c.conn.SendClient(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalResize{
		TerminalResize: &fleetv1.TerminalResize{AgentId: t.agentID, Cols: uint32(cols), Rows: uint32(rows)},
	}})
}

// Detach stops streaming; the agent keeps running.
func (t *Terminal) Detach(ctx context.Context) error {
	select {
	case <-t.done:
		return nil
	default:
	}
	_, err := unary(ctx, t.c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Detach{Detach: &fleetv1.DetachRequest{AgentId: t.agentID}}},
		(*fleetv1.ServerMessage).GetDetach)
	t.finish("detached")
	return err
}

// finish ends the terminal once, unregistering it from the client.
func (t *Terminal) finish(reason string) {
	t.once.Do(func() {
		t.c.mu.Lock()
		if t.c.terms[t.agentID] == t {
			delete(t.c.terms, t.agentID)
		}
		t.c.mu.Unlock()
		t.reason = reason
		t.out.close()
		close(t.done)
	})
}
