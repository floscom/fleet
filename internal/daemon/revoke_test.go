package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/identity"
	"fleet/internal/wire"
)

// A connection whose device was revoked after it authenticated (the revoke
// raced its auth and did not close it) must not be served.
func TestRevokedDeviceConnectionRefused(t *testing.T) {
	store, err := identity.OpenDeviceStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{devices: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &conn{d: d, raw: srv, wc: wire.NewConn(srv), ctx: ctx, cancel: cancel}
	c.setAuthed("revoked-device")

	got := make(chan *fleetv1.ServerMessage, 1)
	go func() {
		m, _ := wire.NewConn(cli).RecvServer()
		got <- m
	}()
	err = c.dispatch(7, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_GetInfo{GetInfo: &fleetv1.GetInfoRequest{}}})
	if !errors.Is(err, errClose) {
		t.Fatalf("dispatch = %v, want errClose", err)
	}
	select {
	case m := <-got:
		if m.GetId() != 7 || m.GetError().GetCode() != codeUnauth {
			t.Fatalf("reply %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
}
