package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleet/internal/discovery"
)

// signed sends method path to ts signed with key for server id target. A
// nonce of "" fetches a fresh one.
func signed(t *testing.T, ts *httptest.Server, key, target, nonce, method, path, body string) (int, string) {
	t.Helper()
	if nonce == "" {
		nonce = getNonce(t, ts)
	}
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	mac := peerMAC(key, target, nonce, method, req.URL.RequestURI(), []byte(body))
	req.Header.Set("Authorization", peerScheme+nonce+"."+hex.EncodeToString(mac))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func getNonce(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	var n struct{ Nonce string }
	if code := api(t, ts, "GET", "/api/nonce", "", "", &n); code != 200 || len(n.Nonce) != 32 {
		t.Fatalf("nonce: %d %+v", code, n)
	}
	return n.Nonce
}

func TestPeerAuth(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	key, _ := LoadOrCreateToken(keyPath)
	src := newFakeSource()
	src.id = "b-id"
	ts := startServerOpts(t, Options{Source: src, TokenPath: filepath.Join(dir, "web-token"), KeyPath: keyPath})

	if code, body := signed(t, ts, key, "b-id", "", "GET", "/api/roots", ""); code != 200 || !strings.Contains(body, `"code"`) {
		t.Fatalf("signed GET: %d %s", code, body)
	}
	if code, _ := signed(t, ts, key, "b-id", "", "GET", "/api/session", ""); code != 200 {
		t.Fatalf("signed session: %d", code)
	}
	if code, body := signed(t, ts, key, "b-id", "", "POST", "/api/roots", `{"path":"/srv/x","name":"x"}`); code != 201 || len(src.added) != 1 {
		t.Fatalf("signed POST: %d %s", code, body)
	}
	if code, body := signed(t, ts, key, "b-id", "", "PUT", "/api/roots/my%20root", `{"path":"/srv/new","name":"new","trust":true}`); code != 200 || len(src.updated) != 1 || src.updated[0].name != "my root" || !src.updated[0].req.Trust {
		t.Fatalf("signed PUT: %d %s", code, body)
	}
	if code, _ := signed(t, ts, key, "b-id", "", "DELETE", "/api/roots/my%20root", ""); code != 204 || src.removed[0] != "my root" {
		t.Fatalf("signed DELETE: %d %v", code, src.removed)
	}
	var sess struct{ Admin bool }
	if code, body := signed(t, ts, key, "b-id", "", "GET", "/api/session", ""); code != 200 || json.Unmarshal([]byte(body), &sess) != nil || !sess.Admin {
		t.Fatalf("signed session: %d %s", code, body)
	}

	other, _ := RotateToken(filepath.Join(dir, "other"))
	n := getNonce(t, ts)
	// In order: "replayed nonce" reuses the nonce of "first use".
	for _, tc := range []struct{ name, key, target, nonce, method, path, body string }{
		{"wrong key", other, "b-id", "", "GET", "/api/roots", ""},
		{"other target", key, "a-id", "", "GET", "/api/roots", ""},
		{"unknown nonce", key, "b-id", strings.Repeat("0", 32), "GET", "/api/roots", ""},
		{"browser-only", key, "b-id", "", "GET", "/api/join", ""},
		{"no forwarding", key, "b-id", "", "GET", "/api/hosts/c-id/roots", ""},
		{"first use", key, "b-id", n, "GET", "/api/fs?path=/", ""},
		{"replayed nonce", key, "b-id", n, "GET", "/api/fs?path=/", ""},
		{"lowercase method", key, "b-id", "", "get", "/api/roots", ""},
	} {
		name := tc.name
		code, body := signed(t, ts, tc.key, tc.target, tc.nonce, tc.method, tc.path, tc.body)
		want := 401
		switch name {
		case "first use":
			want = 200
		case "browser-only", "no forwarding":
			want = 404 // the admin routes do not have them
		case "lowercase method":
			want = 405 // signed fine, but no such route
		}
		if code != want {
			t.Errorf("%s: %d %s, want %d", name, code, body, want)
		}
	}

	// A signature covers the body and the query.
	nonce := getNonce(t, ts)
	req, _ := http.NewRequest("POST", ts.URL+"/api/roots", strings.NewReader(`{"path":"/etc"}`))
	mac := peerMAC(key, "b-id", nonce, "POST", "/api/roots", []byte(`{"path":"/srv/x"}`))
	req.Header.Set("Authorization", peerScheme+nonce+"."+hex.EncodeToString(mac))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("tampered body: %v %v", resp, err)
	}
	if code, _ := signedQuery(t, ts, key, "/api/fs?path=/", "/api/fs?path=/etc"); code != 401 {
		t.Fatalf("tampered query: %d", code)
	}
	if len(src.added) != 1 {
		t.Fatalf("source changed by a bad signature: %v", src.added)
	}

	// No key file: nothing is signed right, and /api/nonce says so.
	ts2 := startServerOpts(t, Options{Source: newFakeSource(), KeyPath: filepath.Join(dir, "missing")})
	if code, _ := signed(t, ts2, key, "self-id", "", "GET", "/api/roots", ""); code != 401 {
		t.Fatalf("no key file: %d", code)
	}
	ts3 := startServerOpts(t, Options{Source: newFakeSource()})
	if code := api(t, ts3, "GET", "/api/nonce", "", "", nil); code != 404 {
		t.Fatalf("nonce without KeyPath: %d", code)
	}
}

// signedQuery signs signedPath but requests sentPath.
func signedQuery(t *testing.T, ts *httptest.Server, key, signedPath, sentPath string) (int, string) {
	nonce := getNonce(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+sentPath, nil)
	mac := peerMAC(key, "b-id", nonce, "GET", signedPath, nil)
	req.Header.Set("Authorization", peerScheme+nonce+"."+hex.EncodeToString(mac))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestNonces(t *testing.T) {
	var n nonces
	for i := 0; i < maxNonces; i++ {
		if _, ok := n.issue(); !ok {
			t.Fatalf("nonce %d refused", i)
		}
	}
	if _, ok := n.issue(); ok {
		t.Fatal("issued more than maxNonces")
	}
	for k := range n.m {
		n.m[k] = time.Now().Add(-time.Second) // all expired
	}
	x, ok := n.issue()
	if !ok || len(n.m) != 1 {
		t.Fatalf("expired nonces not pruned: %d", len(n.m))
	}
	if !n.take(x) || n.take(x) {
		t.Fatal("a nonce must work exactly once")
	}
}

// hostPort splits an httptest URL.
func hostPort(t *testing.T, ts *httptest.Server) (string, int) {
	u, _ := url.Parse(ts.URL)
	h, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	return h, port
}

func TestHostProxy(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	LoadOrCreateToken(keyPath)
	// b holds the key, c has another one, d has none, e is gone.
	bSrc := newFakeSource()
	bSrc.id = "b-id"
	b := startServerOpts(t, Options{Source: bSrc, KeyPath: keyPath})
	cKey := filepath.Join(dir, "c-key")
	RotateToken(cKey)
	cSrc := newFakeSource()
	cSrc.id = "c-id"
	c := startServerOpts(t, Options{Source: cSrc, KeyPath: cKey})
	dSrc := newFakeSource()
	dSrc.id = "d-id"
	d := startServerOpts(t, Options{Source: dSrc})
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	var found []discovery.Found
	for _, p := range []struct {
		id string
		ts *httptest.Server
	}{{"b-id", b}, {"c-id", c}, {"d-id", d}, {"e-id", gone}} {
		h, port := hostPort(t, p.ts)
		// A refused address first: the next one is tried.
		found = append(found, discovery.Found{ServerID: p.id, Name: strings.TrimSuffix(p.id, "-id"), Addrs: []string{"127.0.0.2", h}, WebPort: port})
	}
	tokPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokPath)
	a := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: tokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) { return found, nil }})

	// Peers are known once a browser is connected and the browse is done.
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
			break
		}
	}

	var sess struct{ Admin bool }
	if code := api(t, a, "GET", "/api/hosts/b-id/session", tok, "", &sess); code != 200 || !sess.Admin {
		t.Fatalf("b session: %d %+v", code, sess)
	}
	var roots struct{ Roots []Root }
	if code := api(t, a, "GET", "/api/hosts/b-id/roots", tok, "", &roots); code != 200 || len(roots.Roots) != 1 {
		t.Fatalf("b roots: %d %+v", code, roots)
	}
	var us struct{ Agents []AgentUsage }
	if code := api(t, a, "GET", "/api/hosts/b-id/usage", tok, "", &us); code != 200 || len(us.Agents) != 1 || us.Agents[0].Plan != "max 20x" {
		t.Fatalf("usage on b: %d %+v", code, us)
	}
	var listing Dir
	if code := api(t, a, "GET", "/api/hosts/b-id/fs?path="+url.QueryEscape(dir), tok, "", &listing); code != 200 || listing.Path == "" {
		t.Fatalf("b fs: %d %+v", code, listing)
	}
	var added struct{ Root Root }
	if code := api(t, a, "POST", "/api/hosts/b-id/roots", tok, `{"path":"/srv/api","name":"api"}`, &added); code != 201 || added.Root.Name != "api" {
		t.Fatalf("b add: %d %+v", code, added)
	}
	var updated struct{ Root Root }
	if code := api(t, a, "PUT", "/api/hosts/b-id/roots/my%20root", tok, `{"path":"/srv/new","name":"new","adapters":["claude"],"trust":true}`, &updated); code != 200 || updated.Root.Name != "new" || !updated.Root.Trust {
		t.Fatalf("b update: %d %+v", code, updated)
	}
	if len(bSrc.updated) != 1 || bSrc.updated[0].name != "my root" || bSrc.updated[0].req.Path != "/srv/new" || !reflect.DeepEqual(bSrc.updated[0].req.Adapters, []string{"claude"}) {
		t.Fatalf("b got updates %v", bSrc.updated)
	}
	if code := api(t, a, "DELETE", "/api/hosts/b-id/roots/my%20root", tok, "", nil); code != 204 {
		t.Fatalf("b remove: %d", code)
	}
	if len(bSrc.added) != 1 || bSrc.added[0].Path != "/srv/api" || !reflect.DeepEqual(bSrc.removed, []string{"my root"}) {
		t.Fatalf("b got %v %v", bSrc.added, bSrc.removed)
	}
	// Agents on b: started, typed into and stopped from here.
	var run struct{ Agent Agent }
	if code := api(t, a, "POST", "/api/hosts/b-id/agents", tok, `{"adapter":"codex","root":"code"}`, &run); code != 201 || run.Agent.ID != "a1" {
		t.Fatalf("b run: %d %+v", code, run)
	}
	if code := api(t, a, "POST", "/api/hosts/b-id/agents/a1/input", tok, `{"text":"go on","submit":true}`, nil); code != 200 {
		t.Fatalf("b input: %d", code)
	}
	if code := api(t, a, "POST", "/api/hosts/b-id/agents/a1/stop", tok, `{}`, nil); code != 200 {
		t.Fatalf("b stop: %d", code)
	}
	if len(bSrc.runs) != 1 || bSrc.runs[0].Adapter != "codex" || len(bSrc.inputs) != 1 || bSrc.inputs[0].text != "go on" || len(bSrc.stops) != 1 {
		t.Fatalf("b got runs %v inputs %v stops %v", bSrc.runs, bSrc.inputs, bSrc.stops)
	}

	// Machines without our key, unreachable ones, unknown ones, and routes
	// that are not forwarded.
	var e struct {
		Error     string
		NotJoined bool
	}
	for _, id := range []string{"c-id", "d-id"} {
		if code := api(t, a, "POST", "/api/hosts/"+id+"/roots", tok, `{"path":"/srv/api"}`, &e); code != 403 || !e.NotJoined {
			t.Fatalf("%s: %d %+v", id, code, e)
		}
		e.NotJoined = false
	}
	if len(cSrc.added)+len(dSrc.added) != 0 {
		t.Fatal("a machine without the key was changed")
	}
	if code := api(t, a, "GET", "/api/hosts/e-id/roots", tok, "", &e); code != 502 || e.NotJoined {
		t.Fatalf("unreachable: %d %+v", code, e)
	}
	for _, p := range []string{"/api/hosts/zz-id/roots", "/api/hosts/self-id/roots", "/api/hosts/b-id/join", "/api/hosts/b-id/hosts/c-id/roots", "/api/hosts/b-id/nonce"} {
		if code := api(t, a, "GET", p, tok, "", &e); code != 404 {
			t.Fatalf("%s: %d %+v", p, code, e)
		}
	}
	// The browser token is needed; a signed request is not forwarded.
	if code := api(t, a, "GET", "/api/hosts/b-id/roots", "", "", &e); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	key, _ := readToken(keyPath)
	if code, _ := signed(t, a, key, "self-id", "", "GET", "/api/hosts/b-id/roots", ""); code != 404 {
		t.Fatalf("signed forward: %d", code)
	}

	var join struct{ Command string }
	if code := api(t, a, "GET", "/api/join", tok, "", &join); code != 200 || join.Command != "fleet start --join "+key {
		t.Fatalf("join: %d %+v", code, join)
	}
}

func TestValidKey(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for k, want := range map[string]bool{good: true, "": false, good[:63]: false, strings.ToUpper(good): false, good[:62] + "zz": false} {
		if ValidKey(k) != want {
			t.Errorf("ValidKey(%q) = %v", k, !want)
		}
	}
	path := filepath.Join(t.TempDir(), "k")
	if err := SetToken(path, "nope"); err == nil {
		t.Fatal("SetToken took a bad key")
	}
	if err := SetToken(path, good); err != nil {
		t.Fatal(err)
	}
	if got, _ := readToken(path); got != good {
		t.Fatalf("stored %q", got)
	}
}
