package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/update"
)

// fakeRelease serves a repository whose latest release is tag, and makes
// the daemon update from it quickly.
func fakeRelease(t *testing.T, tag string) {
	t.Helper()
	target := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	script := "#!/bin/sh\necho 'fleet " + tag + " (protocol v1)'\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: target + "/fleet", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	zw.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + target + ".tar.gz\n"))
	})
	mux.HandleFunc("GET /releases/download/"+tag+"/"+target+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	first, poll, src := updateFirst, updatePoll, updateSource
	t.Cleanup(func() {
		updateFirst, updatePoll, updateSource = first, poll, src
		srv.Close()
	})
	updateFirst, updatePoll, updateSource = 10*time.Millisecond, 20*time.Millisecond, update.Source{URL: srv.URL}
}

func updateReq(r *fleetv1.UpdateRequest) *fleetv1.ClientMessage {
	return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Update{Update: r}}
}

func TestAutoUpdateRestartsOnTheNewRelease(t *testing.T) {
	fakeRelease(t, "v0.4.0")
	e := newEnv(t)
	e.stop()
	e.version = "v0.3.0"
	e.binary = filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(e.binary, []byte("old"), 0o755)
	e.start()
	select {
	case err := <-e.done:
		e.cancel = nil
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("Run = %v, want ErrRestart", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not restart")
	}
	if b, _ := os.ReadFile(e.binary); !strings.Contains(string(b), "fleet v0.4.0") {
		t.Errorf("binary = %q", b)
	}
}

func TestAutoUpdateOff(t *testing.T) {
	fakeRelease(t, "v0.4.0")
	e := newEnv(t)
	c := e.dialUnix()
	st := c.ok(updateReq(&fleetv1.UpdateRequest{})).GetUpdate().GetStatus()
	if !st.Auto || st.Version != "test" || !strings.Contains(st.Unsupported, "development build") {
		t.Fatalf("status = %v", st)
	}
	st = c.ok(updateReq(&fleetv1.UpdateRequest{SetAuto: true})).GetUpdate().GetStatus()
	if st.Auto {
		t.Fatal("auto still on")
	}
	if cfg, err := config.Load(); err != nil || cfg.AutoUpdateOn() {
		t.Fatalf("config.toml: auto_update on (%v)", err)
	}
	// A development build looks, but does not install.
	st = c.ok(updateReq(&fleetv1.UpdateRequest{Install: true})).GetUpdate().GetStatus()
	if st.Latest != "v0.4.0" || st.Installed != "" || st.Error != "" || st.CheckedAtMs == 0 {
		t.Fatalf("status = %v", st)
	}

	// Off: a release that could update does not, until asked to.
	e.stop()
	e.version = "v0.3.0"
	e.binary = filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(e.binary, []byte("old"), 0o755)
	e.start()
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(e.binary); string(b) != "old" {
		t.Fatal("updated with auto_update off")
	}
	c = e.dialUnix()
	st = c.ok(updateReq(&fleetv1.UpdateRequest{Install: true})).GetUpdate().GetStatus()
	if st.Installed != "v0.4.0" || st.Unsupported != "" || st.Auto {
		t.Fatalf("status = %v", st)
	}
	select {
	case err := <-e.done:
		e.cancel = nil
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("Run = %v, want ErrRestart", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not restart")
	}
}

func TestBusyReason(t *testing.T) {
	a := &agentRec{ID: "a1", Name: "a", State: fleetv1.AgentState_AGENT_STATE_WORKING}
	m := awakeManager(t, &config.Config{}, a)
	if got := m.busyReason(); got != "a works" {
		t.Errorf("working: %q", got)
	}
	a.State = fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
	m.asks = map[string]*pendingAsk{"x": {}}
	if got := m.busyReason(); !strings.Contains(got, "question") {
		t.Errorf("asking: %q", got)
	}
	a.State = fleetv1.AgentState_AGENT_STATE_IDLE
	m.asks = nil
	if got := m.busyReason(); got != "" {
		t.Errorf("idle: %q", got)
	}
}
