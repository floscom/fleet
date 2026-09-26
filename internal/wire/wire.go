// Package wire implements fleet's framing: each message is a 4-byte
// big-endian length followed by a protobuf-encoded message.
package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
)

// MaxFrameSize is the largest frame either side accepts.
const MaxFrameSize = 4 << 20

// ErrFrameTooLarge is returned when a peer announces a frame above MaxFrameSize.
var ErrFrameTooLarge = errors.New("wire: frame too large")

// WriteFrame marshals m and writes it as one length-prefixed frame.
func WriteFrame(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("wire: marshal: %w", err)
	}
	if len(b) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one length-prefixed frame into m.
func ReadFrame(r io.Reader, m proto.Message) error {
	return readFrame(r, m, MaxFrameSize)
}

// PreAuthMaxFrameSize is a frame limit for peers that have not authenticated
// yet: handshake messages are tiny.
const PreAuthMaxFrameSize = 64 << 10

// readFrame reads one frame of at most limit bytes. Frames above
// PreAuthMaxFrameSize are buffered as their bytes arrive rather than
// allocated up front, so a bare length header cannot pin memory.
func readFrame(r io.Reader, m proto.Message, limit uint32) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > limit {
		return ErrFrameTooLarge
	}
	var buf []byte
	if n <= PreAuthMaxFrameSize {
		buf = make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	} else {
		var b bytes.Buffer
		if _, err := io.CopyN(&b, r, int64(n)); err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		buf = b.Bytes()
	}
	return proto.Unmarshal(buf, m)
}

// Conn wraps a stream with concurrency-safe framed writes. Reads are expected
// to happen from a single goroutine.
type Conn struct {
	rw    io.ReadWriteCloser
	mu    sync.Mutex
	limit atomic.Uint32 // read frame limit; 0 means MaxFrameSize
}

// SetMaxFrameSize limits the size of frames read from now on (capped at
// MaxFrameSize). Safe for concurrent use.
func (c *Conn) SetMaxFrameSize(n uint32) { c.limit.Store(min(n, MaxFrameSize)) }

func (c *Conn) readLimit() uint32 {
	if n := c.limit.Load(); n != 0 {
		return n
	}
	return MaxFrameSize
}

// NewConn wraps rw.
func NewConn(rw io.ReadWriteCloser) *Conn { return &Conn{rw: rw} }

// SendServer writes a ServerMessage. Safe for concurrent use.
func (c *Conn) SendServer(m *fleetv1.ServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteFrame(c.rw, m)
}

// SendClient writes a ClientMessage. Safe for concurrent use.
func (c *Conn) SendClient(m *fleetv1.ClientMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteFrame(c.rw, m)
}

// RecvServer reads the next ServerMessage.
func (c *Conn) RecvServer() (*fleetv1.ServerMessage, error) {
	m := &fleetv1.ServerMessage{}
	if err := readFrame(c.rw, m, c.readLimit()); err != nil {
		return nil, err
	}
	return m, nil
}

// RecvClient reads the next ClientMessage.
func (c *Conn) RecvClient() (*fleetv1.ClientMessage, error) {
	m := &fleetv1.ClientMessage{}
	if err := readFrame(c.rw, m, c.readLimit()); err != nil {
		return nil, err
	}
	return m, nil
}

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rw.Close() }

// Underlying returns the wrapped stream (e.g. to inspect TLS state).
func (c *Conn) Underlying() io.ReadWriteCloser { return c.rw }
