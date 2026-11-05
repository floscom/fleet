package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fleet/internal/hub"
	"fleet/internal/skills"
	"fleet/internal/team"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testHub is a hub behind an httptest server, with a switch to tamper
// with the heads it hands out.
type testHub struct {
	url    string
	tamper atomic.Pointer[func(*team.Head)]
}

func newTestHub(t *testing.T, open bool) *testHub {
	t.Helper()
	s, err := hub.New(hub.Options{DataDir: t.TempDir(), Open: open, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	h := &testHub{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := h.tamper.Load()
		if f == nil || r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/bundles") {
			s.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			if strings.HasSuffix(r.URL.Path, "/bundles") {
				var l team.HeadList
				json.Unmarshal(body, &l)
				for _, hd := range l.Heads {
					(*f)(hd)
				}
				body, _ = json.Marshal(l)
			} else {
				var hd team.Head
				json.Unmarshal(body, &hd)
				(*f)(&hd)
				body, _ = json.Marshal(&hd)
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	h.url = ts.URL
	return h
}

// fleet is one daemon: a skill store and its gateway.
type fleet struct {
	t     *testing.T
	g     *Gateway
	store *skills.Store
	home  string
}

func newFleet(t *testing.T, name string) *fleet {
	t.Helper()
	home := t.TempDir()
	st, err := skills.Open(skills.Options{
		Dir: filepath.Join(home, "skills"), State: filepath.Join(home, "skills.json"), ID: name + "-id", Name: name, Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := Open(Options{Dir: filepath.Join(home, "teams"), Skills: st, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return &fleet{t: t, g: g, store: st, home: home}
}

// reopen starts the fleet's gateway afresh from its files.
func (f *fleet) reopen() {
	f.t.Helper()
	g, err := Open(Options{Dir: filepath.Join(f.home, "teams"), Skills: f.store, Log: quiet})
	if err != nil {
		f.t.Fatal(err)
	}
	f.g = g
}

// write puts a file into skill name and lets the store take it.
func (f *fleet) write(name, file, content string) {
	f.t.Helper()
	p := filepath.Join(f.store.Dir(), name, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.settle()
}

// settle has the store take what changed in its folder (a change is taken
// once it held still for a scan).
func (f *fleet) settle() {
	f.store.Scan(context.Background())
	f.store.Scan(context.Background())
}

func (f *fleet) read(name, file string) string {
	b, err := os.ReadFile(filepath.Join(f.store.Dir(), name, file))
	if err != nil {
		return ""
	}
	return string(b)
}

func (f *fleet) sync() {
	f.t.Helper()
	if err := f.g.Sync(context.Background(), ""); err != nil {
		f.t.Fatalf("sync: %v", err)
	}
}

func (f *fleet) view() TeamView {
	f.t.Helper()
	v, err := f.g.Team("")
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fleet) skill(ref string) SkillView {
	f.t.Helper()
	for _, s := range f.view().Skills {
		if s.Ref == ref {
			return s
		}
	}
	f.t.Fatalf("no skill %s in %+v", ref, f.view().Skills)
	return SkillView{}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
}

// join brings f into a's team with an invite of a's.
func join(t *testing.T, a, f *fleet, name string) {
	t.Helper()
	ctx := context.Background()
	inv, _, err := a.g.Invite(ctx, "", 0)
	must(t, err)
	v, err := f.g.Join(ctx, inv, name)
	must(t, err)
	if v.Status != StatusWaiting {
		t.Fatalf("joiner status %q, want waiting", v.Status)
	}
	f.sync()
	if s := f.view().Status; s != StatusWaiting {
		t.Fatalf("before an admin acted: %q", s)
	}
	a.sync() // the admin adds it
	f.sync()
	if s := f.view().Status; s != StatusOK {
		t.Fatalf("after the admin acted: %q (%s)", s, f.view().Error)
	}
}

func TestCreateNeedsCode(t *testing.T) {
	h := newTestHub(t, false)
	a := newFleet(t, "a")
	_, err := a.g.Create(context.Background(), h.url, "", "codeline", "alice")
	var ge *Error
	if !errors.As(err, &ge) || ge.Kind != Forbidden {
		t.Fatalf("create without a code: %v", err)
	}
	if len(a.g.Teams()) != 0 {
		t.Fatal("a team was kept")
	}
}

func TestTeam(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t, true)
	a, b, c := newFleet(t, "a"), newFleet(t, "b"), newFleet(t, "c")

	// Create, invite, join: the invite is the approval.
	v, err := a.g.Create(ctx, h.url, "", "codeline", "alice")
	must(t, err)
	if v.Name != "codeline" || !v.Admin || len(v.Members) != 1 || v.Members[0].Name != "alice" {
		t.Fatalf("created: %+v", v)
	}
	join(t, a, b, "bob")
	bv := b.view()
	if bv.Name != "codeline" || bv.Admin || len(bv.Members) != 2 {
		t.Fatalf("b's view: %+v", bv)
	}
	if len(a.view().Invites) != 0 {
		t.Fatalf("a used invite is still open: %+v", a.view().Invites)
	}

	// Publish: only own skills.
	a.write("git-commit", "SKILL.md", skillMD("git-commit", "Commit with conventional messages", "v1\n"))
	must(t, a.g.Publish(ctx, "", "git-commit"))
	if p := a.view().Published; len(p) != 1 || p[0].Status != "published" {
		t.Fatalf("published: %+v", p)
	}
	b.sync()
	s := b.skill("alice/git-commit")
	if s.Description != "Commit with conventional messages" || s.Subscribed || s.Installed != "" || s.Files != 1 {
		t.Fatalf("before subscribing: %+v", s)
	}
	if b.read("git-commit", "SKILL.md") != "" {
		t.Fatal("installed without a subscription")
	}

	// Subscribe installs it.
	must(t, b.g.Subscribe(ctx, "", "alice/git-commit"))
	if !strings.Contains(b.read("git-commit", "SKILL.md"), "v1") {
		t.Fatal("not installed")
	}
	e, _ := b.store.Entry("git-commit")
	if e.Source == nil || e.Source.OwnerName != "alice" || e.Source.TeamName != "codeline" {
		t.Fatalf("source: %+v", e.Source)
	}
	if err := b.g.Publish(ctx, "", "git-commit"); err == nil {
		t.Fatal("published someone else's skill")
	}

	// Update.
	a.write("git-commit", "SKILL.md", skillMD("git-commit", "Commit with conventional messages", "v2\n"))
	a.sync()
	b.sync()
	if !strings.Contains(b.read("git-commit", "SKILL.md"), "v2") {
		t.Fatalf("not updated: %q", b.read("git-commit", "SKILL.md"))
	}

	// Pin: the update waits, with a diff, until applied.
	must(t, b.g.Pin(ctx, "", "alice/git-commit", true))
	a.write("git-commit", "SKILL.md", skillMD("git-commit", "Commit with conventional messages", "v3\n"))
	a.write("git-commit", "scripts/check.sh", "#!/bin/sh\necho ok\n")
	a.sync()
	b.sync()
	if !strings.Contains(b.read("git-commit", "SKILL.md"), "v2") {
		t.Fatal("a pinned skill was updated")
	}
	s = b.skill("alice/git-commit")
	if !s.Pinned || s.Pending == nil {
		t.Fatalf("pending: %+v", s)
	}
	d, err := b.g.Diff("", "alice/git-commit")
	must(t, err)
	if !strings.Contains(d.Text, "-v2\n+v3\n") || !strings.Contains(d.Text, "+++ b/scripts/check.sh") || len(d.Files) != 2 {
		t.Fatalf("diff: %+v\n%s", d.Files, d.Text)
	}
	must(t, b.g.Apply(ctx, "", "git-commit"))
	if !strings.Contains(b.read("git-commit", "SKILL.md"), "v3") || b.read("git-commit", "scripts/check.sh") == "" {
		t.Fatal("apply did not install the update")
	}
	if s := b.skill("alice/git-commit"); s.Pending != nil || !s.Pinned {
		t.Fatalf("after apply: %+v", s)
	}
	must(t, b.g.Pin(ctx, "", "alice/git-commit", false))

	// A change made on b forks it: it stops updating.
	b.write("git-commit", "SKILL.md", skillMD("git-commit", "mine now", "bob's\n"))
	a.write("git-commit", "SKILL.md", skillMD("git-commit", "Commit with conventional messages", "v4\n"))
	a.sync()
	b.sync()
	if !strings.Contains(b.read("git-commit", "SKILL.md"), "bob's") {
		t.Fatal("a forked skill was updated")
	}
	if s := b.skill("alice/git-commit"); s.Forked != "git-commit" || s.Installed != "" {
		t.Fatalf("forked: %+v", s)
	}

	// A wildcard subscription; a name taken here installs as <owner>-<name>.
	b.write("review", "SKILL.md", skillMD("review", "bob's own", "x\n"))
	a.write("review", "SKILL.md", skillMD("review", "alice's review", "r1\n"))
	a.write("lint", "SKILL.md", skillMD("lint", "lint it", "l1\n"))
	must(t, a.g.Publish(ctx, "", "review"))
	must(t, a.g.Publish(ctx, "", "lint"))
	must(t, b.g.Subscribe(ctx, "", "alice/*"))
	b.sync()
	if !strings.Contains(b.read("alice-review", "SKILL.md"), "r1") || !strings.Contains(b.read("review", "SKILL.md"), "bob's own") {
		t.Fatal("the name clash was not resolved")
	}
	if !strings.Contains(b.read("alice-review", "SKILL.md"), `name: "alice-review"`) {
		t.Fatalf("SKILL.md not renamed: %q", b.read("alice-review", "SKILL.md"))
	}
	if b.read("lint", "SKILL.md") == "" {
		t.Fatal("wildcard did not install lint")
	}

	// A pinned skill installed under another name diffs without the rename.
	must(t, b.g.Pin(ctx, "", "alice/review", true))
	a.write("review", "SKILL.md", skillMD("review", "alice's review", "r2\n"))
	a.sync()
	b.sync()
	d, err = b.g.Diff("", "alice-review")
	must(t, err)
	if strings.Contains(d.Text, "-name:") || strings.Contains(d.Text, "+name:") || !strings.Contains(d.Text, "-r1\n+r2\n") {
		t.Fatalf("diff of a renamed skill:\n%s", d.Text)
	}

	// Unpublish removes it from subscribers.
	must(t, a.g.Unpublish(ctx, "", "lint"))
	b.sync()
	if b.read("lint", "SKILL.md") != "" {
		t.Fatal("an unpublished skill stayed")
	}

	// Deleted here: unsubscribed, not installed again, until subscribed to
	// by name.
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d1\n"))
	must(t, a.g.Publish(ctx, "", "deploy"))
	b.sync()
	if b.read("deploy", "SKILL.md") == "" {
		t.Fatal("wildcard did not install deploy")
	}
	os.RemoveAll(filepath.Join(b.store.Dir(), "deploy"))
	b.settle()
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d2\n"))
	a.sync()
	b.sync()
	if b.read("deploy", "SKILL.md") != "" {
		t.Fatal("a skill deleted here came back")
	}
	if s := b.skill("alice/deploy"); !s.Dropped || s.Subscribed {
		t.Fatalf("deleted: %+v", s)
	}
	must(t, b.g.Subscribe(ctx, "", "alice/deploy"))
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d2") {
		t.Fatal("subscribing by name did not take it back")
	}

	// A third member; removing it starts a new epoch, the owner re-wraps,
	// the others go on and the removed one gets nothing new.
	join(t, a, c, "carol")
	must(t, c.g.Subscribe(ctx, "", "alice/deploy"))
	if c.read("deploy", "SKILL.md") == "" {
		t.Fatal("c did not install deploy")
	}
	epoch := a.view().Epoch
	must(t, a.g.Remove(ctx, "", "carol"))
	if av := a.view(); av.Epoch != epoch+1 || len(av.Members) != 2 {
		t.Fatalf("after removing carol: epoch %d members %d", av.Epoch, len(av.Members))
	}
	b.sync()
	if bv := b.view(); bv.Epoch != epoch+1 {
		t.Fatalf("b's epoch %d", bv.Epoch)
	}
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d3\n"))
	a.sync()
	b.sync()
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d3") {
		t.Fatal("b got nothing after the new epoch")
	}
	for _, s := range b.view().Skills {
		if s.Problem != "" {
			t.Fatalf("after re-wrap: %+v", s)
		}
	}
	if err := c.g.Sync(ctx, ""); err != nil {
		t.Fatalf("removed member's sync: %v", err)
	}
	if cv := c.view(); cv.Status != StatusRemoved {
		t.Fatalf("removed member: %+v", cv.Status)
	}
	if !strings.Contains(c.read("deploy", "SKILL.md"), "d2") {
		t.Fatalf("the removed member lost or updated what it had: %q", c.read("deploy", "SKILL.md"))
	}

	// A gateway started afresh carries on from its files.
	b.reopen()
	b.sync()
	if s := b.skill("alice/git-commit"); s.Forked != "git-commit" {
		t.Fatalf("after reopening: %+v", s)
	}

	// Admins: the last one cannot leave or be demoted.
	if err := a.g.Leave(ctx, ""); err == nil {
		t.Fatal("the last admin left")
	}
	if err := a.g.SetAdmin(ctx, "", "alice", false); err == nil {
		t.Fatal("the last admin was demoted")
	}
	must(t, a.g.SetAdmin(ctx, "", "bob", true))
	b.sync()
	if !b.view().Admin {
		t.Fatal("bob is no admin")
	}

	// Leaving: the hub drops the leaver's bundles, an admin drops it from
	// the roster with a new epoch. Its installed skills stay.
	must(t, b.g.Leave(ctx, ""))
	if len(b.g.Teams()) != 0 {
		t.Fatal("b still lists the team")
	}
	if b.read("deploy", "SKILL.md") == "" {
		t.Fatal("leaving removed an installed skill")
	}
	epoch = a.view().Epoch
	a.sync()
	if av := a.view(); len(av.Members) != 1 || av.Epoch != epoch+1 {
		t.Fatalf("after bob left: %+v", av.Members)
	}
}

func TestTamperedHeadIsSkipped(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t, true)
	a, b := newFleet(t, "a"), newFleet(t, "b")
	_, err := a.g.Create(ctx, h.url, "", "codeline", "alice")
	must(t, err)
	join(t, a, b, "bob")
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d1\n"))
	must(t, a.g.Publish(ctx, "", "deploy"))
	must(t, b.g.Subscribe(ctx, "", "alice/*"))
	b.sync()
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d1") {
		t.Fatal("not installed")
	}

	// A head whose data ref the hub changed does not verify.
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d2\n"))
	a.sync()
	tamper := func(hd *team.Head) { hd.DataRef = strings.Repeat("0", 64) }
	h.tamper.Store(&tamper)
	b.sync()
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d1") {
		t.Fatal("a tampered head was installed")
	}

	// Nor does an older version offered again (a rollback): kept at d2.
	h.tamper.Store(nil)
	b.sync()
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d2") {
		t.Fatal("the good head was not installed")
	}
	var old *team.Head
	b.g.list()[0].op.Lock()
	for _, bd := range b.g.list()[0].st.Bundles {
		old = bd.Head
	}
	b.g.list()[0].op.Unlock()
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d3\n"))
	a.sync()
	b.sync()
	stale := *old
	stale.Version--
	tb := b.g.list()[0]
	tb.op.Lock()
	err = tb.takeHead(ctx, &stale)
	tb.op.Unlock()
	must(t, err)
	if !strings.Contains(b.read("deploy", "SKILL.md"), "d3") {
		t.Fatal("rolled back")
	}
}

func TestRunFollowsEvents(t *testing.T) {
	storePoll, retryPublish = 50*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newTestHub(t, true)
	a, b := newFleet(t, "a"), newFleet(t, "b")
	_, err := a.g.Create(ctx, h.url, "", "codeline", "alice")
	must(t, err)
	join(t, a, b, "bob")
	done := make(chan struct{}, 2)
	for _, f := range []*fleet{a, b} {
		go func() { f.g.Run(ctx); done <- struct{}{} }()
	}
	defer func() { cancel(); <-done; <-done }()
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
		}
	}

	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d1\n"))
	must(t, a.g.Publish(ctx, "", "deploy"))
	must(t, b.g.Subscribe(ctx, "", "alice/*"))
	// A change the store takes is published, and the event brings it to b.
	a.write("deploy", "SKILL.md", skillMD("deploy", "ship it", "d2\n"))
	wait("the update on b", func() bool { return strings.Contains(b.read("deploy", "SKILL.md"), "d2") })
	// A skill deleted by its owner is withdrawn.
	os.RemoveAll(filepath.Join(a.store.Dir(), "deploy"))
	a.settle()
	wait("the skill gone from b", func() bool { return b.read("deploy", "SKILL.md") == "" })
	if p := a.view().Published; len(p) != 0 {
		t.Fatalf("still published: %+v", p)
	}
}

func TestDescribe(t *testing.T) {
	for in, want := range map[string]string{
		"---\nname: x\ndescription: plain one\n---\n":                   "plain one",
		"---\ndescription: \"quoted: yes\"\n---\n":                      "quoted: yes",
		"---\ndescription: >\n  folded\n  lines\nname: x\n---\n":        "folded lines",
		"---\r\ndescription: crlf\r\n---\r\n":                           "crlf",
		"# no front matter\ndescription: no\n":                          "",
		"---\nname: x\n---\n":                                           "",
		"\ufeff---\ndescription: bom\n---\n":                            "bom",
		"---\ndescription: starts here\n  and goes on\nother: 1\n---\n": "starts here and goes on",
	} {
		if got := describe([]byte(in)); got != want {
			t.Errorf("describe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenamed(t *testing.T) {
	in := "---\nname: review\ndescription: d\n---\nbody name: review\n"
	want := "---\nname: \"alice-review\"\ndescription: d\n---\nbody name: review\n"
	if got := string(renamed([]byte(in), "alice-review")); got != want {
		t.Fatalf("renamed: %q", got)
	}
	if got := string(renamed([]byte("no front matter\n"), "x")); got != "no front matter\n" {
		t.Fatalf("renamed without front matter: %q", got)
	}
}

func TestUnified(t *testing.T) {
	a := lines([]byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n"))
	b := lines([]byte("1\n2\nthree\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n15\n16"))
	want := `--- a/f
+++ b/f
@@ -1,6 +1,6 @@
 1
 2
-3
+three
 4
 5
 6
@@ -11,5 +11,5 @@
 11
 12
 13
-14
 15
+16
\ No newline at end of file
`
	if got := unified("a/f", "b/f", a, b); got != want {
		t.Fatalf("unified:\n%s\nwant:\n%s", got, want)
	}
	// Every edit script turns a into b.
	for _, c := range [][2]string{{"", "x\n"}, {"x\n", ""}, {"a\nb\nc\n", "c\nb\na\n"}, {"a\na\na\n", "a\n"}, {"same\n", "same\n"}} {
		x, y := lines([]byte(c[0])), lines([]byte(c[1]))
		var gotA, gotB bytes.Buffer
		for _, e := range lineDiff(x, y) {
			if e.op != '+' {
				gotA.WriteString(e.line)
			}
			if e.op != '-' {
				gotB.WriteString(e.line)
			}
		}
		if gotA.String() != c[0] || gotB.String() != c[1] {
			t.Errorf("lineDiff(%q, %q) does not add up", c[0], c[1])
		}
	}
}

func TestStateIsPrivate(t *testing.T) {
	h := newTestHub(t, true)
	a := newFleet(t, "a")
	v, err := a.g.Create(context.Background(), h.url, "", "codeline", "alice")
	must(t, err)
	for _, f := range []string{keyFile, stateFile} {
		fi, err := os.Stat(filepath.Join(a.home, "teams", v.ID, f))
		must(t, err)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", f, fi.Mode().Perm())
		}
	}
}

func TestAdminsRace(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t, true)
	a, b, c := newFleet(t, "a"), newFleet(t, "b"), newFleet(t, "c")
	_, err := a.g.Create(ctx, h.url, "", "codeline", "alice")
	must(t, err)
	join(t, a, b, "bob")
	must(t, a.g.SetAdmin(ctx, "", "bob", true))
	b.sync()
	// a adds carol while b still has the roster before: b's next change
	// loses the race, fetches the chain and goes on top of it.
	join(t, a, c, "carol")
	if err := b.g.SetAdmin(ctx, "", "alice", false); err != nil {
		t.Fatalf("set admin after a lost race: %v", err)
	}
	a.sync()
	av := a.view()
	if av.Admin || len(av.Members) != 3 {
		t.Fatalf("after the race: %+v", av.Members)
	}
	// Only b is an admin now, and it cannot be demoted.
	if err := b.g.SetAdmin(ctx, "", "bob", false); err == nil {
		t.Fatal("demoted the last admin")
	}
	if err := a.g.Remove(ctx, "", "carol"); err == nil {
		t.Fatal("a member who is no admin removed someone")
	}
}

func TestOwnerRewrapsAfterEpoch(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t, true)
	a, b, c, d := newFleet(t, "a"), newFleet(t, "b"), newFleet(t, "c"), newFleet(t, "d")
	_, err := a.g.Create(ctx, h.url, "", "codeline", "alice")
	must(t, err)
	join(t, a, b, "bob")
	join(t, a, c, "carol")
	join(t, a, d, "dave")
	b.write("tool", "SKILL.md", skillMD("tool", "a tool", "t1\n"))
	must(t, b.g.Publish(ctx, "", "tool"))
	must(t, c.g.Subscribe(ctx, "", "bob/tool"))
	if c.read("tool", "SKILL.md") == "" {
		t.Fatal("not installed")
	}

	// A new epoch: until bob's fleet re-wraps, carol opens bob's head with
	// the key she kept; dave, removed, gets nothing new.
	must(t, a.g.Remove(ctx, "", "dave"))
	c.sync()
	if s := c.skill("bob/tool"); s.Problem != "" {
		t.Fatalf("before the re-wrap: %+v", s)
	}
	b.sync() // re-wraps under the new key
	if p := b.view().Published; p[0].Status != "published" {
		t.Fatalf("after the re-wrap: %+v", p)
	}
	b.write("tool", "SKILL.md", skillMD("tool", "a tool", "t2\n"))
	b.sync()
	c.sync()
	if !strings.Contains(c.read("tool", "SKILL.md"), "t2") {
		t.Fatal("carol got nothing after the new epoch")
	}
	tc := c.g.list()[0]
	tc.op.Lock()
	epoch, keys := tc.st.Roster.Epoch, len(tc.st.Keys)
	tc.op.Unlock()
	if keys != 1 {
		t.Fatalf("carol keeps %d keys at epoch %d; the old one is needed no more", keys, epoch)
	}
}
