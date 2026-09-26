package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

type rwc struct{ io.ReadWriter }

func (rwc) Close() error { return nil }

func header(n uint32) []byte {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], n)
	return h[:]
}

func TestRoundTripLargeFrame(t *testing.T) {
	var buf bytes.Buffer
	want := &fleetv1.ClientMessage{Id: 9, Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Text: strings.Repeat("x", 1<<20)}}}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := NewConn(rwc{&buf}).RecvClient()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetId() != 9 || len(got.GetSendText().GetText()) != 1<<20 {
		t.Fatalf("got id %d, %d bytes", got.GetId(), len(got.GetSendText().GetText()))
	}
}

func TestMaxFrameSize(t *testing.T) {
	c := NewConn(rwc{bytes.NewBuffer(header(PreAuthMaxFrameSize + 1))})
	c.SetMaxFrameSize(PreAuthMaxFrameSize)
	if _, err := c.RecvClient(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	c = NewConn(rwc{bytes.NewBuffer(header(MaxFrameSize + 1))})
	if _, err := c.RecvClient(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

// A header announcing a huge frame must not allocate the whole frame
// before its bytes arrive.
func TestHeaderAloneDoesNotAllocate(t *testing.T) {
	in := append(header(MaxFrameSize), "short"...)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := ReadFrame(bytes.NewReader(in), &fleetv1.ClientMessage{})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > MaxFrameSize/4 {
		t.Fatalf("allocated %d bytes for a 5-byte body", d)
	}
}
