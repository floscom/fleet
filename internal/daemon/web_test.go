package daemon

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/web"
)

func TestLanPort(t *testing.T) {
	if lanPort(nil) != 0 {
		t.Fatal("nil listener advertised")
	}
	for addr, lan := range map[string]bool{"127.0.0.1:0": false, "[::1]:0": false, "0.0.0.0:0": true} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("%s: %v", addr, err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if got := lanPort(ln); (got == port) != lan || (!lan && got != 0) {
			t.Errorf("lanPort(%s) = %d, port %d, want lan=%v", addr, got, port, lan)
		}
		ln.Close()
	}
}

func TestWebError(t *testing.T) {
	for err, status := range map[error]int{
		errf(codeNotFound, "no root"): http.StatusNotFound,
		errf(codeInvalid, "bad"):      http.StatusBadRequest,
		errf(codeDenied, "no"):        http.StatusForbidden,
	} {
		var we *web.Error
		if !errors.As(webError(err), &we) || we.Status != status || we.Msg != err.Error() {
			t.Errorf("webError(%v) = %v, want status %d", err, webError(err), status)
		}
	}
	internal := errors.New("disk full")
	if webError(internal) != internal || webError(nil) != nil {
		t.Fatal("internal errors must stay internal")
	}
}

func TestWebUpdateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	old := config.Root{Name: "code", Path: t.TempDir(), Trust: true, Adapters: []string{"test"}}
	d := &daemon{
		cfg:  &config.Config{Roots: []config.Root{old}},
		opts: Options{Adapters: adapter.NewRegistry(testAdapter{})},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sub := &subscriber{ch: make(chan *fleetv1.Event, 4)}
	d.agents = &manager{d: d, subs: map[*subscriber]struct{}{sub: {}}}
	source := &webSource{d: d}
	newPath := t.TempDir()
	req := &fleetv1.AddRootRequest{Name: "renamed", Path: newPath}
	r, err := source.UpdateRoot("code", req)
	if err != nil || r.Name != "renamed" || r.Path != newPath || r.Trust || len(r.Adapters) != 0 {
		t.Fatalf("update root = %+v, %v", r, err)
	}
	loaded, err := config.Load()
	if err != nil || !reflect.DeepEqual(loaded.Roots, d.cfg.Roots) {
		t.Fatalf("saved roots = %+v, %v; want %+v", loaded, err, d.cfg.Roots)
	}
	select {
	case ev := <-sub.ch:
		roots := ev.GetRootsChanged().GetRoots()
		if len(roots) != 1 || roots[0].Name != "renamed" || roots[0].Path != newPath {
			t.Fatalf("broadcast roots = %+v", roots)
		}
	default:
		t.Fatal("root update did not reach dashboard subscribers")
	}
	before := d.config().Roots
	for _, tc := range []struct {
		name   string
		req    *fleetv1.AddRootRequest
		status int
	}{
		{"code", req, http.StatusNotFound},
		{"renamed", &fleetv1.AddRootRequest{Path: "relative"}, http.StatusBadRequest},
		{"renamed", &fleetv1.AddRootRequest{Path: newPath, Adapters: []string{"missing"}}, http.StatusNotFound},
	} {
		_, err := source.UpdateRoot(tc.name, tc.req)
		var we *web.Error
		if !errors.As(err, &we) || we.Status != tc.status || !reflect.DeepEqual(d.cfg.Roots, before) || len(sub.ch) != 0 {
			t.Fatalf("invalid update %+v = %v; roots = %+v", tc, err, d.cfg.Roots)
		}
	}
	// Force the atomic save's rename to fail. The live roots must remain
	// intact, and subscribers must not see a failed update.
	if err := os.Remove(filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = source.UpdateRoot("renamed", &fleetv1.AddRootRequest{Name: "again", Path: old.Path, Trust: true})
	if err == nil || !reflect.DeepEqual(d.cfg.Roots, before) || len(sub.ch) != 0 {
		t.Fatalf("failed save = %v; roots = %+v", err, d.cfg.Roots)
	}
}
