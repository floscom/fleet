package web

import (
	"context"
	"encoding/json"
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

func TestSendAnyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	LoadOrCreateToken(keyPath)

	bSrc := newFakeSource()
	bSrc.id = "b0c1d2e3"
	bSrc.uploadDir = filepath.Join(dir, "b-uploads")
	b := startServerOpts(t, Options{Source: bSrc, KeyPath: keyPath})
	h, port := hostPort(t, b)
	found := []discovery.Found{
		{ServerID: "b0c1d2e3", Name: "macbook", Host: "macbook.local.", Addrs: []string{h}, WebPort: port},
		{ServerID: "c9", Name: "nodash", Host: "nodash.local.", Addrs: []string{h}},
	}

	want := "the report"
	file := filepath.Join(dir, "report.txt")
	os.WriteFile(file, []byte(want), 0o644)
	aSrc := newFakeSource()
	aSrc.id = "a-id"
	tokPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokPath)
	a := startServerOpts(t, Options{Source: aSrc, TokenPath: tokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) { return found, nil }})

	// No dashboard is open: the peer list comes from a browse on demand.
	var peers struct {
		Peers    []Peer
		Complete bool
	}
	if code := api(t, a, "GET", "/api/peers", tok, "", &peers); code != 200 || !peers.Complete || len(peers.Peers) != 3 || !peers.Peers[0].Self {
		t.Fatalf("peers: %d %+v", code, peers)
	}

	type reply struct {
		Host, Name, Path, Dir string
		Size                  int64
		Error                 string
	}
	send := func(host, into string) (int, reply) {
		t.Helper()
		var r reply
		body, _ := json.Marshal(map[string]string{"path": file, "host": host, "dir": into})
		return api(t, a, "POST", "/api/send", tok, string(body), &r), r
	}

	// By name, host name, short host name and id prefix, in any case.
	for _, ref := range []string{"macbook", "MACBOOK", "macbook.local", "MacBook.local.", "b0c1"} {
		code, r := send(ref, "")
		if code != 200 || r.Host != "b0c1d2e3" || r.Name != "macbook" || r.Dir != "" || filepath.Dir(r.Path) != bSrc.uploadDir {
			t.Fatalf("send to %q: %d %+v", ref, code, r)
		}
		if got, _ := os.ReadFile(r.Path); string(got) != want {
			t.Fatalf("send to %q saved %q", ref, got)
		}
	}

	// Into a folder there, under the file's own name, never replacing one.
	into := filepath.Join(dir, "inbox")
	os.Mkdir(into, 0o755)
	code, r := send("macbook", into)
	if code != 200 || r.Path != filepath.Join(into, "report.txt") || r.Dir != into {
		t.Fatalf("send into folder: %d %+v", code, r)
	}
	if got, _ := os.ReadFile(r.Path); string(got) != want {
		t.Fatalf("saved %q", got)
	}
	if code, r := send("macbook", into); code != 409 || !strings.Contains(r.Error, "already exists") {
		t.Fatalf("second send into folder: %d %+v", code, r)
	}
	if entries, _ := os.ReadDir(into); len(entries) != 1 {
		t.Fatalf("inbox holds %d files, want 1 (no partial left)", len(entries))
	}
	// "~" is the receiving machine's home.
	t.Setenv("HOME", dir)
	if code, r := send("macbook", "~/inbox/"); code != 409 || !strings.Contains(r.Error, into) {
		t.Fatalf("send into ~/inbox: %d %+v", code, r)
	}
	if code, r := send("macbook", filepath.Join(dir, "missing")); code != 400 {
		t.Fatalf("send into a missing folder: %d %+v", code, r)
	}
	if code, r := send("macbook", "inbox"); code != 400 {
		t.Fatalf("relative folder: %d %+v", code, r)
	}

	// Machines it cannot send to.
	for _, c := range []struct {
		ref  string
		code int
		msg  string
	}{
		{"", 404, "name the machine"},
		{"nowhere", 404, "macbook (b0c1d2e3)"},
		{"a-id", 400, "this machine"},
		{"studio", 400, "this machine"},
		{"nodash", 409, "no dashboard"},
	} {
		if code, r := send(c.ref, ""); code != c.code || !strings.Contains(r.Error, c.msg) {
			t.Fatalf("send to %q: %d %+v, want %d %q", c.ref, code, r, c.code, c.msg)
		}
	}

	// Not reachable with the fleet key: one fleet cannot make another
	// pass files on.
	cTokPath := filepath.Join(dir, "c-web-token")
	cTok, _ := LoadOrCreateToken(cTokPath)
	hA, portA := hostPort(t, a)
	c := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: cTokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) {
			return []discovery.Found{{ServerID: "a-id", Name: "a", Addrs: []string{hA}, WebPort: portA}}, nil
		}})
	waitPeers(t, c)
	var e struct{ Error string }
	body, _ := json.Marshal(map[string]string{"path": file, "host": "macbook"})
	if code := api(t, c, "POST", "/api/hosts/a-id/send", cTok, string(body), &e); code != 404 {
		t.Fatalf("forwarded /api/send: %d %+v", code, e)
	}
}
