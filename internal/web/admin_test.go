package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

// api sends method path with an optional bearer token and JSON body, and
// decodes the JSON reply into out (if non-nil).
func api(t *testing.T, ts *httptest.Server, method, path, token, body string, out any) int {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s %s: content type %q", method, path, ct)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	tok, err := LoadOrCreateToken(path)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q: %v", tok, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if again, _ := LoadOrCreateToken(path); again != tok {
		t.Fatal("LoadOrCreateToken replaced an existing token")
	}
	if rotated, _ := RotateToken(path); rotated == tok || len(rotated) != 64 {
		t.Fatalf("rotated token %q", rotated)
	}
}

func TestAdminAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	src := newFakeSource()
	ts := startServerToken(t, src, path)

	var sess struct{ Admin bool }
	// No token file yet: nobody is admin, not even with an empty token.
	if code := api(t, ts, "GET", "/api/session", "x", "", &sess); code != 200 || sess.Admin {
		t.Fatalf("session without token file: %d %+v", code, sess)
	}
	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		admin bool
	}{{"", false}, {"wrong", false}, {tok[:63], false}, {tok, true}} {
		if code := api(t, ts, "GET", "/api/session", tc.token, "", &sess); code != 200 || sess.Admin != tc.admin {
			t.Fatalf("session with %q: %d %+v", tc.token, code, sess)
		}
	}

	// Every admin route refuses a missing or wrong token.
	for _, rt := range [][2]string{{"GET", "/api/fs"}, {"GET", "/api/adapters"}, {"GET", "/api/roots"}, {"POST", "/api/roots"},
		{"DELETE", "/api/roots/code"}, {"GET", "/api/join"}, {"GET", "/api/hosts/peer-id/fs"}} {
		for _, bad := range []string{"", "wrong"} {
			var e struct{ Error string }
			if code := api(t, ts, rt[0], rt[1], bad, `{"path":"/"}`, &e); code != 401 || e.Error == "" {
				t.Fatalf("%s %s with %q: %d %+v", rt[0], rt[1], bad, code, e)
			}
		}
	}
	if len(src.added)+len(src.removed) != 0 {
		t.Fatalf("source changed without auth: %v %v", src.added, src.removed)
	}

	// A cookie is not a credential.
	req, _ := http.NewRequest("POST", ts.URL+"/api/roots", strings.NewReader(`{"path":"/"}`))
	req.AddCookie(&http.Cookie{Name: "token", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("cookie auth: %d", resp.StatusCode)
	}

	// Rotating the token signs the old one out without a restart.
	if _, err := RotateToken(path); err != nil {
		t.Fatal(err)
	}
	if api(t, ts, "GET", "/api/session", tok, "", &sess); sess.Admin {
		t.Fatal("old token still admin after rotation")
	}
}

func TestUnlock(t *testing.T) {
	dir := t.TempDir()
	tokPath, keyPath := filepath.Join(dir, "web-token"), filepath.Join(dir, "fleet-key")
	ts := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: tokPath, KeyPath: keyPath})
	unlock := func(body string) (int, string) {
		t.Helper()
		var v struct{ Token, Error string }
		code := api(t, ts, "POST", "/api/unlock", "", body, &v)
		return code, v.Token
	}

	key := strings.Repeat("5b", 32)
	// No key file yet: nothing unlocks.
	if code, tok := unlock(`{"key":"` + key + `"}`); code != 403 || tok != "" {
		t.Fatalf("unlock without key file: %d %q", code, tok)
	}
	must(t, SetToken(keyPath, key))

	// The fleet key unlocks and creates the admin token.
	code, tok := unlock(`{"key":" ` + key + `\n"}`)
	if code != 200 || !ValidKey(tok) || tok == key {
		t.Fatalf("unlock with fleet key: %d %q", code, tok)
	}
	if want, _ := readToken(tokPath); want != tok {
		t.Fatalf("unlock gave %q, token file holds %q", tok, want)
	}
	var sess struct{ Admin bool }
	if api(t, ts, "GET", "/api/session", tok, "", &sess); !sess.Admin {
		t.Fatal("token from unlock is not admin")
	}
	// So does the admin token itself, also one set by hand.
	if code, again := unlock(`{"key":"` + tok + `"}`); code != 200 || again != tok {
		t.Fatalf("unlock with admin token: %d %q", code, again)
	}
	must(t, os.WriteFile(tokPath, []byte("Ab3\n"), 0o600))
	if code, again := unlock(`{"key":"Ab3"}`); code != 200 || again != "Ab3" {
		t.Fatalf("unlock with a short admin token: %d %q", code, again)
	}
	if code, _ := unlock(`{"key":"ab3"}`); code != 403 {
		t.Fatalf("unlock with a short token in the wrong case: %d", code)
	}

	for _, bad := range []string{`{"key":""}`, `{"key":"` + key[:63] + `"}`, `{"key":"` + strings.ToUpper(key) + `"}`, `{}`} {
		if code, tok := unlock(bad); code != 403 || tok != "" {
			t.Fatalf("unlock %s: %d %q", bad, code, tok)
		}
	}
	if code, _ := unlock(`not json`); code != 400 {
		t.Fatalf("unlock with bad JSON: %d", code)
	}
	if code := api(t, ts, "GET", "/api/unlock", "", "", nil); code != 405 {
		t.Fatalf("GET /api/unlock: %d", code)
	}

	// A daemon without admin access has nothing to unlock.
	off := startServerOpts(t, Options{Source: newFakeSource(), KeyPath: keyPath})
	if code := api(t, off, "POST", "/api/unlock", "", `{"key":"`+key+`"}`, nil); code != 404 {
		t.Fatalf("unlock without TokenPath: %d", code)
	}
}

func TestAdminRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	src := newFakeSource()
	ts := startServerToken(t, src, path)

	var added struct{ Root Root }
	code := api(t, ts, "POST", "/api/roots", tok, `{"path":"/srv/code","name":" api ","adapters":["claude"],"trust":true}`, &added)
	if code != 201 || added.Root.Name != "api" || added.Root.Path != "/srv/code" || !added.Root.Trust {
		t.Fatalf("add: %d %+v", code, added)
	}
	want := &fleetv1.AddRootRequest{Path: "/srv/code", Name: "api", Adapters: []string{"claude"}, Trust: true}
	if len(src.added) != 1 || !reflect.DeepEqual(src.added[0].Adapters, want.Adapters) ||
		src.added[0].Path != want.Path || src.added[0].Name != want.Name || !src.added[0].Trust {
		t.Fatalf("source got %v", src.added)
	}

	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/roots", tok, `{nope`, &e); code != 400 {
		t.Fatalf("bad JSON: %d %+v", code, e)
	}
	// User errors keep their status and message; others are hidden.
	src.addErr = &Error{Status: 400, Msg: "/srv/code is already root \"api\""}
	if code := api(t, ts, "POST", "/api/roots", tok, `{"path":"/srv/code"}`, &e); code != 400 || !strings.Contains(e.Error, "already root") {
		t.Fatalf("duplicate: %d %+v", code, e)
	}
	src.addErr = io.ErrUnexpectedEOF
	if code := api(t, ts, "POST", "/api/roots", tok, `{"path":"/srv/code"}`, &e); code != 500 || e.Error != "internal error" {
		t.Fatalf("internal: %d %+v", code, e)
	}

	if code := api(t, ts, "DELETE", "/api/roots/my%20root", tok, "", nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	src.removeErr = &Error{Status: 404, Msg: `unknown root "gone"`}
	if code := api(t, ts, "DELETE", "/api/roots/gone", tok, "", &e); code != 404 || !strings.Contains(e.Error, "gone") {
		t.Fatalf("remove unknown: %d %+v", code, e)
	}
	if !reflect.DeepEqual(src.removed, []string{"my root", "gone"}) {
		t.Fatalf("removed %v", src.removed)
	}
	var roots struct{ Roots []Root }
	if code := api(t, ts, "GET", "/api/roots", tok, "", &roots); code != 200 || len(roots.Roots) != 1 || roots.Roots[0].Name != "code" {
		t.Fatalf("GET /api/roots: %d %+v", code, roots)
	}

	var ad struct{ Adapters []Adapter }
	if code := api(t, ts, "GET", "/api/adapters", tok, "", &ad); code != 200 || len(ad.Adapters) != 1 || ad.Adapters[0].ID != "claude" {
		t.Fatalf("adapters: %d %+v", code, ad)
	}
}

func TestAdminListDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, newFakeSource(), path)

	dir := t.TempDir()
	for _, d := range []string{"beta", "Alpha", ".hidden", "repo/.git", "wt"} {
		must(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(dir, "wt", ".git"), nil, 0o644)) // a worktree's .git file
	must(t, os.WriteFile(filepath.Join(dir, "file.txt"), nil, 0o644))
	must(t, os.Symlink(filepath.Join(dir, "beta"), filepath.Join(dir, "link")))
	must(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "broken")))
	must(t, os.Symlink(filepath.Join(dir, "file.txt"), filepath.Join(dir, "filelink")))
	real, _ := filepath.EvalSymlinks(dir)

	var got Dir
	if code := api(t, ts, "GET", "/api/fs?path="+dir, tok, "", &got); code != 200 {
		t.Fatalf("list: %d", code)
	}
	want := []DirEntry{{"Alpha", false}, {"beta", false}, {"link", false}, {"repo", true}, {"wt", true}}
	if got.Path != real || got.Parent != filepath.Dir(real) || !reflect.DeepEqual(got.Entries, want) || got.Truncated {
		t.Fatalf("list: %+v", got)
	}
	api(t, ts, "GET", "/api/fs?hidden=1&path="+dir, tok, "", &got)
	if len(got.Entries) != 6 || got.Entries[0].Name != ".hidden" {
		t.Fatalf("hidden: %+v", got.Entries)
	}
	// Through a symlink, the listing names the real folder.
	api(t, ts, "GET", "/api/fs?path="+filepath.Join(dir, "link"), tok, "", &got)
	if got.Path != filepath.Join(real, "beta") {
		t.Fatalf("symlinked path %q", got.Path)
	}
	api(t, ts, "GET", "/api/fs?path=/", tok, "", &got)
	if got.Path != "/" || got.Parent != "" {
		t.Fatalf("root: %+v", got)
	}
	// No path: the home directory.
	home, _ := os.UserHomeDir()
	api(t, ts, "GET", "/api/fs", tok, "", &got)
	if realHome, _ := filepath.EvalSymlinks(home); got.Path != realHome || got.Home != home {
		t.Fatalf("home: %+v", got)
	}

	var e struct{ Error string }
	for p, status := range map[string]int{
		"relative/dir":                 400,
		filepath.Join(dir, "missing"):  404,
		filepath.Join(dir, "file.txt"): 400,
		filepath.Join(dir, "broken"):   404,
	} {
		if code := api(t, ts, "GET", "/api/fs?path="+p, tok, "", &e); code != status || e.Error == "" {
			t.Errorf("%s: %d %+v, want %d", p, code, e, status)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
