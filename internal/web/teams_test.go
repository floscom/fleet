package web

import (
	"io"
	"log/slog"
	"os"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"fleet/internal/gateway"
	teamhub "fleet/internal/hub"
	"fleet/internal/skills"
)

func TestTeamRoutes(t *testing.T) {
	dir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := teamhub.New(teamhub.Options{DataDir: filepath.Join(dir, "hub"), Open: true, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(h)
	defer hs.Close()
	st, err := skills.Open(skills.Options{Dir: filepath.Join(dir, "skills"), State: filepath.Join(dir, "skills.json"), ID: "a-id", Name: "a", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.Open(gateway.Options{Dir: filepath.Join(dir, "teams"), Skills: st, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	tokenPath, keyPath := filepath.Join(dir, "web-token"), filepath.Join(dir, "fleet-key")
	tok, _ := LoadOrCreateToken(tokenPath)
	key, _ := LoadOrCreateToken(keyPath)
	src := newFakeSource()
	src.id = "a-id"
	ts := startServerOpts(t, Options{Source: src, TokenPath: tokenPath, KeyPath: keyPath, Skills: st, Teams: g})

	// The admin token is needed; the fleet key does not reach teams.
	if code := api(t, ts, "GET", "/api/teams", "", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := signed(t, ts, key, "a-id", "", "GET", "/api/teams", ""); code != 404 {
		t.Fatalf("signed with the fleet key: %d", code)
	}
	var list struct{ Teams []gateway.TeamView }
	if code := api(t, ts, "GET", "/api/teams", tok, "", &list); code != 200 || list.Teams == nil || len(list.Teams) != 0 {
		t.Fatalf("list: %d %+v", code, list)
	}

	var created struct{ Team gateway.TeamView }
	body := `{"hub":"` + hs.URL + `","name":"codeline","me":"alice"}`
	if code := api(t, ts, "POST", "/api/teams", tok, body, &created); code != 201 || created.Team.Name != "codeline" || !created.Team.Admin {
		t.Fatalf("create: %d %+v", code, created)
	}
	var e struct{ Error string }
	if code := api(t, ts, "POST", "/api/teams", tok, `{"hub":"ftp://x","name":"x","me":"y"}`, &e); code != 400 || e.Error == "" {
		t.Fatalf("create with a bad hub: %d %+v", code, e)
	}

	var inv struct {
		Invite    string
		ExpiresMs int64
		Team      gateway.TeamView
	}
	if code := api(t, ts, "POST", "/api/teams/codeline/invites", tok, `{"ttl":"2d"}`, &inv); code != 201 || !strings.HasPrefix(inv.Invite, "fleet-team:") || len(inv.Team.Invites) != 1 {
		t.Fatalf("invite: %d %+v", code, inv)
	}
	if code := api(t, ts, "POST", "/api/teams/codeline/invites", tok, `{"ttl":"90d"}`, &e); code != 400 {
		t.Fatalf("invite for 90 days: %d", code)
	}
	if code := api(t, ts, "POST", "/api/teams/"+created.Team.ID[:6]+"/invites", tok, "", &inv); code != 201 {
		t.Fatalf("invite by id prefix, no body: %d", code)
	}

	// Errors carry their kind's status.
	for _, c := range []struct {
		method, path, body string
		code               int
	}{
		{"GET", "/api/teams/nope", "", 404},
		{"POST", "/api/teams/codeline/publish", `{"skill":"missing"}`, 404},
		{"POST", "/api/teams/codeline/subscribe", `{"ref":"nobody"}`, 400},
		{"POST", "/api/teams/codeline/subscribe", `{"ref":"alice/x"}`, 400},
		{"DELETE", "/api/teams/codeline/members/alice", "", 400},
		{"POST", "/api/teams/codeline/members/alice/admin", `{"admin":false}`, 409},
		{"GET", "/api/teams/codeline/diff/alice/x", "", 404},
		{"POST", "/api/teams/codeline/apply", `{"bundle":"x"}`, 404},
		{"POST", "/api/teams/codeline/leave", "", 409},
		{"POST", "/api/teams/join", `{"invite":"nonsense","me":"bob"}`, 400},
	} {
		if code := api(t, ts, c.method, c.path, tok, c.body, &e); code != c.code {
			t.Errorf("%s %s: %d %q, want %d", c.method, c.path, code, e.Error, c.code)
		}
	}

	var one struct{ Team gateway.TeamView }
	if code := api(t, ts, "POST", "/api/teams/codeline/sync", tok, "", &one); code != 200 || one.Team.Status != gateway.StatusOK {
		t.Fatalf("sync: %d %+v", code, one.Team)
	}
}

func TestTeamRoutesWithoutTeams(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokenPath)
	ts := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: tokenPath})
	var e struct{ Error string }
	if code := api(t, ts, "GET", "/api/teams", tok, "", &e); code != 404 || e.Error == "" {
		t.Fatalf("no gateway: %d %+v", code, e)
	}
}

func TestOwnSkills(t *testing.T) {
	dir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := skills.Open(skills.Options{Dir: filepath.Join(dir, "skills"), State: filepath.Join(dir, "skills.json"), ID: "a-id", Name: "a", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for name, md := range map[string]string{
		"review":  "---\nname: review\ndescription: >\n  Reviews a diff\n  for bugs.\n---\n# Review\n",
		"deploy":  "---\nname: deploy\ndescription: \"Ships it\"\n---\n",
		"no-desc": "# Just text\n",
	} {
		if err := os.MkdirAll(filepath.Join(st.Dir(), name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.Dir(), name, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A change is taken once it held still for a scan.
	st.Scan(t.Context())
	st.Scan(t.Context())
	tokenPath, keyPath := filepath.Join(dir, "web-token"), filepath.Join(dir, "fleet-key")
	tok, _ := LoadOrCreateToken(tokenPath)
	key, _ := LoadOrCreateToken(keyPath)
	src := newFakeSource()
	src.id = "a-id"
	ts := startServerOpts(t, Options{Source: src, TokenPath: tokenPath, KeyPath: keyPath, Skills: st})

	if code, _ := signed(t, ts, key, "a-id", "", "GET", "/api/own-skills", ""); code != 404 {
		t.Fatalf("signed with the fleet key: %d", code)
	}
	var got struct {
		Skills []struct {
			Name, Description string
			Size, ChangedMs   int64
		}
	}
	if code := api(t, ts, "GET", "/api/own-skills", tok, "", &got); code != 200 || len(got.Skills) != 3 {
		t.Fatalf("own skills: %d %+v", code, got)
	}
	want := map[string]string{"deploy": "Ships it", "no-desc": "", "review": "Reviews a diff for bugs."}
	for i, s := range got.Skills {
		if s.Description != want[s.Name] || s.Size == 0 || s.ChangedMs == 0 || i > 0 && got.Skills[i-1].Name > s.Name {
			t.Errorf("skill %d: %+v", i, s)
		}
	}

	// Without shared skills: 404.
	ts2 := startServerOpts(t, Options{Source: newFakeSource(), TokenPath: tokenPath})
	var e struct{ Error string }
	if code := api(t, ts2, "GET", "/api/own-skills", tok, "", &e); code != 404 || e.Error == "" {
		t.Fatalf("no skills: %d %+v", code, e)
	}
}
