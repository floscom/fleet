package daemon

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/wire"
)

const (
	// maxAuthFailures closes a connection after this many failed pair/auth attempts.
	maxAuthFailures = 3
	// authFailureDelay slows down guessing.
	authFailureDelay = 300 * time.Millisecond
	// tlsHandshakeTimeout bounds the TLS handshake.
	tlsHandshakeTimeout = 15 * time.Second
	// authTimeout is how long a TLS client may stay unauthenticated.
	authTimeout = 60 * time.Second
	// maxUnauthConns caps concurrent TLS connections that have not
	// authenticated yet; further connections are closed right away.
	maxUnauthConns = 64
)

// conn is one client connection.
type conn struct {
	d     *daemon
	wc    *wire.Conn
	raw   net.Conn
	local bool
	nonce []byte

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	authed   bool
	deviceID string
	failures int
	sub      *subscriber
	// unauth is set while the connection counts against maxUnauthConns.
	unauth bool

	attachMu sync.Mutex
	attaches map[string]*attachment
}

// serve accepts connections until ln is closed.
func (d *daemon) serve(ctx context.Context, ln net.Listener, local bool) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.log.Warn("accept failed", "err", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		c := &conn{d: d, raw: nc, local: local, ctx: cctx, cancel: cancel, attaches: map[string]*attachment{}}
		if !d.addConn(c) {
			cancel()
			nc.Close()
			return
		}
		go c.run()
	}
}

func (d *daemon) addConn(c *conn) bool {
	d.connsMu.Lock()
	defer d.connsMu.Unlock()
	if d.conns == nil {
		return false // shutting down
	}
	d.conns[c] = struct{}{}
	d.connWG.Add(1)
	return true
}

func (d *daemon) removeConn(c *conn) {
	d.connsMu.Lock()
	defer d.connsMu.Unlock()
	if d.conns != nil {
		delete(d.conns, c)
	}
}

// closeAllConns closes every connection and refuses new ones.
func (d *daemon) closeAllConns() {
	d.connsMu.Lock()
	conns := d.conns
	d.conns = nil
	d.connsMu.Unlock()
	for c := range conns {
		c.close()
	}
}

// connsFor returns the authenticated connections of a device.
func (d *daemon) connsFor(deviceID string) []*conn {
	d.connsMu.Lock()
	defer d.connsMu.Unlock()
	var out []*conn
	for c := range d.conns {
		if authed, id := c.identity(); authed && id == deviceID {
			out = append(out, c)
		}
	}
	return out
}

// close tears the connection down; safe to call more than once.
func (c *conn) close() {
	c.cancel()
	c.raw.Close()
}

func (c *conn) identity() (authed bool, deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed, c.deviceID
}

func (c *conn) setAuthed(deviceID string) {
	c.mu.Lock()
	c.authed, c.deviceID, c.failures = true, deviceID, 0
	c.mu.Unlock()
	c.releaseUnauth()
	c.raw.SetReadDeadline(time.Time{})
	if c.wc != nil {
		c.wc.SetMaxFrameSize(wire.MaxFrameSize)
	}
}

// releaseUnauth stops counting c against maxUnauthConns.
func (c *conn) releaseUnauth() {
	c.mu.Lock()
	was := c.unauth
	c.unauth = false
	c.mu.Unlock()
	if was {
		c.d.unauthConns.Add(-1)
	}
}

// fail records a failed pair/auth attempt, delays, and reports whether the
// connection must be closed.
func (c *conn) fail() bool {
	c.mu.Lock()
	c.failures++
	n := c.failures
	c.mu.Unlock()
	select {
	case <-time.After(authFailureDelay):
	case <-c.ctx.Done():
	}
	return n >= maxAuthFailures
}

// run is the per-connection loop: handshake, then requests in order.
func (c *conn) run() {
	d := c.d
	defer d.connWG.Done()
	defer c.cleanup()

	stream := c.raw
	if !c.local {
		if c.d.unauthConns.Add(1) > maxUnauthConns {
			c.d.unauthConns.Add(-1)
			d.log.Warn("too many unauthenticated connections", "remote", c.raw.RemoteAddr())
			return
		}
		c.mu.Lock()
		c.unauth = true
		c.mu.Unlock()
		tc := tls.Server(c.raw, d.server.TLSConfig())
		c.raw.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
		if err := tc.HandshakeContext(c.ctx); err != nil {
			d.log.Debug("tls handshake failed", "remote", c.raw.RemoteAddr(), "err", err)
			return
		}
		c.raw.SetDeadline(time.Time{})
		c.raw.SetReadDeadline(time.Now().Add(authTimeout)) // cleared by setAuthed
		stream = tc
	}
	c.wc = wire.NewConn(stream)
	if !c.local {
		c.wc.SetMaxFrameSize(wire.PreAuthMaxFrameSize)
	}
	go func() { // unblock reads/writes when the daemon stops
		<-c.ctx.Done()
		c.raw.Close()
	}()

	c.nonce = make([]byte, 32)
	if _, err := rand.Read(c.nonce); err != nil {
		return
	}
	cfg := d.config()
	hello := &fleetv1.ServerHello{
		ProtocolVersion: fleetv1.ProtocolVersion_PROTOCOL_VERSION_1,
		ServerId:        d.server.ID,
		ServerName:      cfg.Name,
		DaemonVersion:   d.opts.Version,
		Nonce:           c.nonce,
		AuthRequired:    !c.local,
	}
	if err := c.send(0, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Hello{Hello: hello}}); err != nil {
		return
	}
	if c.local {
		c.setAuthed("")
	}
	for {
		m, err := c.wc.RecvClient()
		if err != nil {
			if !errors.Is(err, io.EOF) && c.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.log.Debug("connection read failed", "err", err)
			}
			return
		}
		if !c.handle(m) {
			return
		}
	}
}

// cleanup releases everything the connection owns. Agents are untouched.
func (c *conn) cleanup() {
	c.close()
	c.releaseUnauth()
	c.d.removeConn(c)
	c.mu.Lock()
	sub := c.sub
	c.sub = nil
	c.mu.Unlock()
	if sub != nil {
		c.d.agents.unsubscribe(sub)
	}
	c.detachAll()
}

// send writes a message with the given id.
func (c *conn) send(id uint64, m *fleetv1.ServerMessage) error {
	m.Id = id
	return c.wc.SendServer(m)
}

// sendError replies with an Error.
func (c *conn) sendError(id uint64, code fleetv1.ErrorCode, msg string) error {
	return c.send(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Error{
		Error: &fleetv1.Error{Code: code, Message: msg},
	}})
}
