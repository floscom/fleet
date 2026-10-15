package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/discovery"
)

// waitPeers blocks until a's dashboard has a complete peer list (the
// browse that Options.Browse feeds it has run at least once).
func waitPeers(t *testing.T, a *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(a), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {a.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	for {
		var m msg
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "peers" && m.Complete {
			return
		}
	}
}

func TestSendFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	LoadOrCreateToken(keyPath)

	// b is the destination: it only takes uploads.
	bSrc := newFakeSource()
	bSrc.id = "b-id"
	bSrc.uploadDir = filepath.Join(dir, "b-uploads")
	b := startServerOpts(t, Options{Source: bSrc, KeyPath: keyPath})
	h, port := hostPort(t, b)
	found := []discovery.Found{{ServerID: "b-id", Name: "b", Addrs: []string{h}, WebPort: port}}

	// a holds the session and the file to send.
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	want := strings.Repeat("payload", 50000) // bigger than one TCP segment
	file := filepath.Join(work, "report.txt")
	if err := os.WriteFile(file, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	aSrc := newFakeSource()
	aSrc.id = "a-id"
	aSrc.agent = &fleetv1.Agent{Id: "a1", Cwd: work}
	tokPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokPath)
	a := startServerOpts(t, Options{Source: aSrc, TokenPath: tokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) { return found, nil }})
	waitPeers(t, a)

	var r struct {
		Host, Name, Path string
		Size             int64
	}
	if code := api(t, a, "POST", "/api/agents/a1/send", tok, `{"path":"`+file+`","host":"b-id"}`, &r); code != 200 {
		t.Fatalf("send: %d %+v", code, r)
	}
	if r.Host != "b-id" || r.Name != "b" || r.Size != int64(len(want)) {
		t.Fatalf("reply = %+v", r)
	}
	if filepath.Dir(r.Path) != bSrc.uploadDir || !strings.HasSuffix(r.Path, "-report.txt") {
		t.Fatalf("remote path %q", r.Path)
	}
	got, err := os.ReadFile(r.Path)
	if err != nil || string(got) != want {
		t.Fatalf("saved %d bytes, err %v", len(got), err)
	}

	// Not a file of this session.
	outside := filepath.Join(dir, "elsewhere.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	var e struct {
		Error string
	}
	if code := api(t, a, "POST", "/api/agents/a1/send", tok, `{"path":"`+outside+`","host":"b-id"}`, &e); code != 404 {
		t.Fatalf("outside file: %d %+v", code, e)
	}

	// An unknown or missing host.
	if code := api(t, a, "POST", "/api/agents/a1/send", tok, `{"path":"`+file+`","host":"zz-id"}`, &e); code != 404 {
		t.Fatalf("unknown host: %d %+v", code, e)
	}
	if code := api(t, a, "POST", "/api/agents/a1/send", tok, `{"path":"`+file+`","host":""}`, &e); code != 400 {
		t.Fatalf("no host: %d %+v", code, e)
	}
	if code := api(t, a, "POST", "/api/agents/a1/send", tok, `{"path":"`+file+`","host":"a-id"}`, &e); code != 400 {
		t.Fatalf("self as host: %d %+v", code, e)
	}

	// No fleet key configured: the feature is off.
	noKeySrc := newFakeSource()
	noKeySrc.agent = &fleetv1.Agent{Id: "a1", Cwd: work}
	noKeyTok := filepath.Join(dir, "no-key-token")
	tok2, _ := LoadOrCreateToken(noKeyTok)
	noKey := startServerToken(t, noKeySrc, noKeyTok)
	if code := api(t, noKey, "POST", "/api/agents/a1/send", tok2, `{"path":"`+file+`","host":"b-id"}`, &e); code != 404 {
		t.Fatalf("no key: %d %+v", code, e)
	}

	// Forwarded through a third dashboard: c asks a to send b the file,
	// the same way any other agent route is forwarded (peers.go).
	cTokPath := filepath.Join(dir, "c-web-token")
	cTok, _ := LoadOrCreateToken(cTokPath)
	hA, portA := hostPort(t, a)
	c := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: cTokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) {
			return []discovery.Found{{ServerID: "a-id", Name: "a", Addrs: []string{hA}, WebPort: portA}}, nil
		}})
	waitPeers(t, c)
	r = struct {
		Host, Name, Path string
		Size             int64
	}{}
	if code := api(t, c, "POST", "/api/hosts/a-id/agents/a1/send", cTok, `{"path":"`+file+`","host":"b-id"}`, &r); code != 200 {
		t.Fatalf("forwarded send: %d %+v", code, r)
	}
	if r.Host != "b-id" || r.Size != int64(len(want)) {
		t.Fatalf("forwarded reply = %+v", r)
	}
	if entries, _ := os.ReadDir(bSrc.uploadDir); len(entries) != 2 {
		t.Fatalf("b holds %d uploads, want 2", len(entries))
	}
}
