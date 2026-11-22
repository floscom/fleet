package web

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

func TestReposRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	src := newFakeSource()
	src.repos = &fleetv1.ListReposResponse{Login: "octo", WorkDir: "/home/octo", Repos: []*fleetv1.Repo{
		{FullName: "octo/app", Private: true, CanPush: true, PushedAtMs: 5, LocalPath: "/home/octo/app"},
		{FullName: "octo/lib", Description: "a library"},
	}}
	ts := startServerToken(t, src, path)

	var list struct {
		Repos   []Repo
		Login   string
		WorkDir string
	}
	if code := api(t, ts, "GET", "/api/repos?q=cli", tok, "", &list); code != 200 || list.Login != "octo" || list.WorkDir != "/home/octo" ||
		len(list.Repos) != 2 || list.Repos[0] != (Repo{FullName: "octo/app", Private: true, CanPush: true, PushedAtMs: 5, LocalPath: "/home/octo/app"}) ||
		list.Repos[1].Description != "a library" || src.repoQueries[0] != "cli" {
		t.Fatalf("list: %d %+v (queries %q)", code, list, src.repoQueries)
	}
	if code := api(t, ts, "GET", "/api/repos", "", "", nil); code != 401 && code != 403 {
		t.Fatalf("list without the token: %d", code)
	}

	var cloned struct {
		Root    Root
		Existed bool
	}
	if code := api(t, ts, "POST", "/api/repos/clone", tok, `{"repo":"octo/app","path":"~/src/app","trust":true,"adapters":["claude"]}`, &cloned); code != 201 ||
		cloned.Root.Path != "~/src/app" || !cloned.Root.Trust || cloned.Existed {
		t.Fatalf("clone: %d %+v", code, cloned)
	}
	if c := src.clones[0]; c.Repo != "octo/app" || c.Path != "~/src/app" || !c.Trust || len(c.Adapters) != 1 || c.Adapters[0] != "claude" {
		t.Fatalf("clone request = %+v", c)
	}

	src.reposErr = &Error{http.StatusServiceUnavailable, "gh is not signed in to GitHub on this host"}
	var e struct{ Error string }
	if code := api(t, ts, "GET", "/api/repos", tok, "", &e); code != 503 || e.Error != src.reposErr.Error() {
		t.Fatalf("signed out: %d %+v", code, e)
	}
	if code := api(t, ts, "POST", "/api/repos/clone", tok, `{"repo":"octo/app"}`, &e); code != 503 {
		t.Fatalf("clone signed out: %d %+v", code, e)
	}

	// Other machines are asked too, and a clone may take its time there.
	if !forwardable(http.MethodGet, "repos") || !forwardable(http.MethodPost, "repos/clone") || forwardable(http.MethodPost, "repos") {
		t.Fatal("GET repos and POST repos/clone must reach other machines, POST repos not")
	}
	if d := forwardTimeout(http.MethodPost, "repos/clone"); d < 10*time.Minute {
		t.Fatalf("a forwarded clone may take %s", d)
	}
}
