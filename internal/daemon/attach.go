package daemon

import (
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"

	fleetv1 "fleet/gen/fleetv1"
)

const (
	// outputChunk is the largest TerminalOutput payload.
	outputChunk = 32 << 10
	// detachTimeout bounds waiting for an attach client to exit.
	detachTimeout = 5 * time.Second
)

// attachment is a tmux client running in a PTY on behalf of a connection.
type attachment struct {
	agentID  string
	readOnly bool
	cmd      *exec.Cmd
	pty      *os.File
	ready    chan struct{} // closed once AttachResponse was sent
	done     chan struct{} // closed when the client has exited and been reaped

	mu       sync.Mutex
	detached bool
}

// stop kills the tmux client (never the session).
func (at *attachment) stop() {
	at.mu.Lock()
	at.detached = true
	at.mu.Unlock()
	at.cmd.Process.Kill()
}

func (at *attachment) wasDetached() bool {
	at.mu.Lock()
	defer at.mu.Unlock()
	return at.detached
}

// attachTarget resolves a live agent for attaching.
func (m *manager) attachTarget(ref string) (id, session string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.find(ref)
	if err != nil {
		return "", "", err
	}
	if !a.live() {
		return "", "", errf(codeInvalid, "agent %s is not running", a.ID)
	}
	return a.ID, a.TmuxSession, nil
}

func (c *conn) handleAttach(id uint64, req *fleetv1.AttachRequest) error {
	agentID, session, err := c.d.agents.attachTarget(req.GetAgent())
	if err != nil {
		return err
	}
	c.attachMu.Lock()
	if _, dup := c.attaches[agentID]; dup {
		c.attachMu.Unlock()
		return errf(codeExists, "already attached to %s on this connection", agentID)
	}
	cmd := c.d.tmux.AttachCommand(session, req.GetReadOnly())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(dim(req.GetCols(), defaultCols)),
		Rows: uint16(dim(req.GetRows(), defaultRows)),
	})
	if err != nil {
		c.attachMu.Unlock()
		return err
	}
	at := &attachment{
		agentID: agentID, readOnly: req.GetReadOnly(), cmd: cmd, pty: f,
		ready: make(chan struct{}), done: make(chan struct{}),
	}
	c.attaches[agentID] = at
	c.attachMu.Unlock()

	go c.pumpTerminal(at)
	err = c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Attach{Attach: &fleetv1.AttachResponse{AgentId: agentID}}})
	close(at.ready)
	return err
}

// pumpTerminal forwards PTY output until the tmux client ends, then reaps it
// and reports TerminalClosed.
func (c *conn) pumpTerminal(at *attachment) {
	defer close(at.done)
	<-at.ready
	buf := make([]byte, outputChunk)
	for {
		n, err := at.pty.Read(buf)
		if n > 0 {
			out := &fleetv1.TerminalOutput{AgentId: at.agentID, Data: append([]byte(nil), buf[:n]...)}
			if c.send(0, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_TerminalOutput{TerminalOutput: out}}) != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	at.cmd.Process.Kill()
	at.cmd.Wait()
	at.pty.Close()

	c.attachMu.Lock()
	if c.attaches[at.agentID] == at {
		delete(c.attaches, at.agentID)
	}
	c.attachMu.Unlock()

	reason := "terminal ended"
	if at.wasDetached() {
		reason = "detached"
	}
	if c.ctx.Err() == nil {
		c.send(0, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_TerminalClosed{
			TerminalClosed: &fleetv1.TerminalClosed{AgentId: at.agentID, Reason: reason},
		}})
	}
}

func (c *conn) attachment(agentID string) (*attachment, error) {
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	at, ok := c.attaches[agentID]
	if !ok {
		return nil, errf(codeNotFound, "not attached to %q", agentID)
	}
	return at, nil
}

func (c *conn) handleInput(id uint64, in *fleetv1.TerminalInput) error {
	at, err := c.attachment(in.GetAgentId())
	if err != nil {
		return err
	}
	if at.readOnly {
		return errf(codeDenied, "terminal of %s is attached read-only", at.agentID)
	}
	if _, err := at.pty.Write(in.GetData()); err != nil {
		return errf(codeInvalid, "terminal of %s is closed", at.agentID)
	}
	return nil
}

func (c *conn) handleResize(id uint64, r *fleetv1.TerminalResize) error {
	at, err := c.attachment(r.GetAgentId())
	if err != nil {
		return err
	}
	if r.GetCols() == 0 || r.GetRows() == 0 {
		return errf(codeInvalid, "cols and rows must be positive")
	}
	if err := pty.Setsize(at.pty, &pty.Winsize{Cols: uint16(dim(r.GetCols(), 0)), Rows: uint16(dim(r.GetRows(), 0))}); err != nil {
		return errf(codeInvalid, "terminal of %s is closed", at.agentID)
	}
	return nil
}

func (c *conn) handleDetach(id uint64, req *fleetv1.DetachRequest) error {
	at, err := c.attachment(req.GetAgentId())
	if err != nil {
		return err
	}
	at.stop()
	select {
	case <-at.done:
	case <-time.After(detachTimeout):
	}
	return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Detach{Detach: &fleetv1.DetachResponse{}}})
}

// detachAll stops every attach client of the connection and waits for them.
func (c *conn) detachAll() {
	c.attachMu.Lock()
	ats := make([]*attachment, 0, len(c.attaches))
	for _, at := range c.attaches {
		ats = append(ats, at)
	}
	c.attachMu.Unlock()
	for _, at := range ats {
		at.stop()
	}
	for _, at := range ats {
		select {
		case <-at.done:
		case <-time.After(detachTimeout):
		}
	}
}
