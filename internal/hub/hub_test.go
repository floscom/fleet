package hub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/internal/team"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testHub is a hub behind an httptest server.
type testHub struct {
	s     *Server
	ts    *httptest.Server
	clock *clock
	dir   string
}

func newTestHub(t *testing.T, opts Options) *testHub {
	t.Helper()
	rateLimit = 100000 // the flows here make many calls; the limiter has its own test
	pollWait = 2 * time.Second
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	opts.now = c.now
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &testHub{s: s, ts: ts, clock: c, dir: opts.DataDir}
}

// founder creates a team on the hub and returns the founder's client, the
// roster and the team key.
func (h *testHub) founder(t *testing.T) (*team.Client, *team.Roster, []byte) {
	t.Helper()
	a, err := team.NewMemberKey(h.ts.URL, team.NewID())
	must(t, err)
	r1, K, err := team.Found(a, "codeline", "florian")
	must(t, err)
	keys, err := team.SealKeys(r1, K)
	must(t, err)
	code, _ := h.s.codes.issue()
	must(t, team.Create(context.Background(), h.ts.URL, code, team.CreateRequest{Roster: r1, Keys: keys}))
	return team.NewClient(a), r1, K
}

// join runs the invite/join/approve flow for a new member and returns the
// joiner's client, the new roster and the joiner's copy of the team key.
func (h *testHub) join(t *testing.T, admin *team.Client, head *team.Roster, K []byte, name string) (*team.Client, *team.Roster, []byte) {
	t.Helper()
	ctx := context.Background()
	inv, err := team.NewInvite(h.ts.URL, admin.Key.Team, admin.Key.ID)
	must(t, err)
	must(t, admin.RegisterInvite(ctx, inv, h.clock.now().Add(team.DefaultInviteTTL)))

	// The joiner only has the invite string.
	parsed, err := team.ParseInvite(inv.String())
	must(t, err)
	b, err := team.NewMemberKey(parsed.Hub, parsed.Team)
	must(t, err)
	jr, err := parsed.NewJoinRequest(b, name)
	must(t, err)
	must(t, team.Join(ctx, parsed, jr))

	// The admin sees the join, checks the proof, adds the member.
	joins, err := admin.Joins(ctx)
	must(t, err)
	var got *team.JoinRequest
	for i := range joins.Joins {
		if joins.Joins[i].Invite == inv.ID() {
			got = &joins.Joins[i]
		}
	}
	if got == nil {
		t.Fatal("join request not listed")
	}
	m, gotName, err := inv.VerifyJoin(*got)
	must(t, err)
	if gotName != name || m.ID != b.ID {
		t.Fatalf("join = %s %q", m.ID, gotName)
	}
	next, err := team.AddMember(head, admin.Key, K, m, gotName)
	must(t, err)
	sealed, err := team.SealTeamKey(next.Team, next.Epoch, K, m)
	must(t, err)
	must(t, admin.PostRoster(ctx, next, []team.SealedKey{sealed}))
	if err := admin.DeleteJoin(ctx, got.ID); !team.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("join request should be gone once the roster lists the member: %v", err)
	}

	// The joiner anchors and gets the key.
	bc := team.NewClient(b)
	rosters, err := bc.Rosters(ctx, 0)
	must(t, err)
	anchor, err := team.Anchor(rosters, b.ID, parsed.Admin)
	must(t, err)
	bHead, err := team.Advance(anchor, rosters)
	must(t, err)
	keys, err := bc.Keys(ctx)
	must(t, err)
	if len(keys) != 1 {
		t.Fatalf("joiner has %d keys, want 1", len(keys))
	}
	bK, err := b.OpenTeamKey(keys[0], bHead)
	must(t, err)
	if !bytes.Equal(bK, K) {
		t.Fatal("joiner's team key differs")
	}
	return bc, next, bK
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantStatus(t *testing.T, err error, status int, what string) {
	t.Helper()
	if !team.IsStatus(err, status) {
		t.Fatalf("%s: got %v, want status %d", what, err, status)
	}
}

func TestEndToEnd(t *testing.T) {
	h := newTestHub(t, Options{})
	ctx := context.Background()
	a, r1, K := h.founder(t)

	// The founder's own view.
	keys, err := a.Keys(ctx)
	must(t, err)
	if len(keys) != 1 {
		t.Fatalf("founder has %d keys", len(keys))
	}
	if _, err := a.Key.OpenTeamKey(keys[0], r1); err != nil {
		t.Fatal(err)
	}
	ev, err := a.Events(ctx, 0)
	must(t, err)
	if len(ev.Events) != 1 || ev.Events[0].Kind != team.EventRoster || ev.Next != 1 {
		t.Fatalf("events after creation: %+v", ev)
	}

	// b joins; the admin saw the join event.
	b, r2, bK := h.join(t, a, r1, K, "beatrice")
	ev, err = a.Events(ctx, ev.Next)
	must(t, err)
	var kinds []string
	for _, e := range ev.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "join,roster,keys" {
		t.Fatalf("admin events after join: %v", kinds)
	}
	// b sees the roster and its own key event but not the join.
	bev, err := b.Events(ctx, 0)
	must(t, err)
	kinds = nil
	for _, e := range bev.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "roster,roster,keys" {
		t.Fatalf("member events: %v", kinds)
	}

	// a publishes a skill; b reads it.
	meta := team.Meta{Kind: "skill", Name: "git-commit", Description: "commit well", Hash: "h", Size: 3, Files: []string{"SKILL.md"}}
	data := bytes.Repeat([]byte("skill tar.gz "), 500)
	bundle := team.NewID()
	head, sealed, err := team.SealBundle(a.Key, bundle, team.NextVersion(0), r2.Epoch, K, meta, data)
	must(t, err)
	wantStatus(t, a.PutHead(ctx, head), http.StatusUnprocessableEntity, "head before data")
	must(t, a.PutData(ctx, head.DataRef, sealed))
	must(t, a.PutData(ctx, head.DataRef, sealed)) // again: fine
	wantStatus(t, mustErr(b.Data(ctx, head.DataRef)), http.StatusNotFound, "unpublished data")
	must(t, a.PutHead(ctx, head))
	heads, err := b.Heads(ctx)
	must(t, err)
	if len(heads) != 1 {
		t.Fatalf("%d heads", len(heads))
	}
	must(t, team.VerifyHead(heads[0], r2))
	blob, err := b.Data(ctx, heads[0].DataRef)
	must(t, err)
	gotMeta, gotData, err := team.OpenBundle(heads[0], bK, blob)
	must(t, err)
	if gotMeta.Name != "git-commit" || !bytes.Equal(gotData, data) {
		t.Fatal("b read something else")
	}
	one, err := b.Head(ctx, bundle)
	must(t, err)
	if one.Version != head.Version {
		t.Fatal("Head differs from Heads")
	}
	// A new version must rise, and b cannot touch a's bundle.
	wantStatus(t, a.PutHead(ctx, head), http.StatusConflict, "same version again")
	bHead, bSealed, err := team.SealBundle(b.Key, bundle, team.NextVersion(head.Version), r2.Epoch, bK, meta, data)
	must(t, err)
	must(t, b.PutData(ctx, bHead.DataRef, bSealed))
	wantStatus(t, b.PutHead(ctx, bHead), http.StatusForbidden, "b publishing a's bundle id")
	// b publishes its own.
	bOwn, bOwnSealed, err := team.SealBundle(b.Key, team.NewID(), team.NextVersion(0), r2.Epoch, bK, meta, data)
	must(t, err)
	must(t, b.PutData(ctx, bOwn.DataRef, bOwnSealed))
	must(t, b.PutHead(ctx, bOwn))

	// c joins; then a removes b: new epoch, b cut off, b's bundle gone.
	c, r3, cK := h.join(t, a, r2, K, "cyrille")
	r4, K2, err := team.RemoveMember(r3, a.Key, K, b.Key.ID)
	must(t, err)
	keys4, err := team.SealKeys(r4, K2)
	must(t, err)
	must(t, a.PostRoster(ctx, r4, keys4))
	wantStatus(t, mustErr(b.Heads(ctx)), http.StatusForbidden, "removed member")
	heads, err = c.Heads(ctx)
	must(t, err)
	if len(heads) != 1 || heads[0].ID != bundle {
		t.Fatalf("after removal heads = %d", len(heads))
	}
	// b's blob is gone from disk, a's is there.
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, bOwn.DataRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removed member's data still stored")
	}
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, head.DataRef)); err != nil {
		t.Fatal("a's data gone")
	}

	// c advances to the new epoch and gets K2; a re-wraps its bundle
	// without uploading again; c opens it with K2.
	rosters, err := c.Rosters(ctx, r3.Seq)
	must(t, err)
	cHead, err := team.Advance(r3, rosters)
	must(t, err)
	if cHead.Seq != r4.Seq || cHead.Epoch != 2 {
		t.Fatalf("c's head seq %d epoch %d", cHead.Seq, cHead.Epoch)
	}
	cKeys, err := c.Keys(ctx)
	must(t, err)
	if len(cKeys) != 1 || cKeys[0].Epoch != 2 {
		t.Fatalf("c's keys: %+v", cKeys)
	}
	cK2, err := c.Key.OpenTeamKey(cKeys[0], cHead)
	must(t, err)
	if bytes.Equal(cK2, cK) {
		t.Fatal("epoch did not change the key")
	}
	re, err := team.Rewrap(head, a.Key, K, team.NextVersion(head.Version), 2, K2)
	must(t, err)
	must(t, a.PutHead(ctx, re))
	heads, err = c.Heads(ctx)
	must(t, err)
	must(t, team.VerifyHead(heads[0], cHead))
	blob, err = c.Data(ctx, heads[0].DataRef)
	must(t, err)
	if _, gotData, err := team.OpenBundle(heads[0], cK2, blob); err != nil || !bytes.Equal(gotData, data) {
		t.Fatalf("rewrapped bundle: %v", err)
	}

	// c leaves: cut off at once, listed for the admin, bundles dropped.
	cOwn, cSealed, err := team.SealBundle(c.Key, team.NewID(), team.NextVersion(0), 2, cK2, meta, data)
	must(t, err)
	must(t, c.PutData(ctx, cOwn.DataRef, cSealed))
	must(t, c.PutHead(ctx, cOwn))
	must(t, c.Leave(ctx))
	wantStatus(t, mustErr(c.Heads(ctx)), http.StatusForbidden, "left member")
	joins, err := a.Joins(ctx)
	must(t, err)
	if len(joins.Left) != 1 || joins.Left[0] != c.Key.ID {
		t.Fatalf("left = %v", joins.Left)
	}
	heads, err = a.Heads(ctx)
	must(t, err)
	if len(heads) != 1 {
		t.Fatalf("after leave heads = %d", len(heads))
	}
	r5, _, err := team.RemoveMember(r4, a.Key, K2, c.Key.ID)
	must(t, err)
	must(t, a.PostRoster(ctx, r5, nil))
	joins, err = a.Joins(ctx)
	must(t, err)
	if len(joins.Left) != 0 {
		t.Fatal("still listed as left after removal")
	}
	// The last admin cannot leave.
	wantStatus(t, a.Leave(ctx), http.StatusConflict, "last admin leaving")

	// Tombstone: the data goes at once.
	tomb, err := team.Tombstone(re, a.Key, team.NextVersion(re.Version))
	must(t, err)
	must(t, a.PutHead(ctx, tomb))
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, head.DataRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tombstoned bundle's data still stored")
	}
	// An upload nobody publishes goes after pendingTTL.
	stray := []byte("stray upload")
	must(t, a.PutData(ctx, team.DataRef(stray), stray))
	h.clock.advance(pendingTTL / 2)
	must(t, a.PutData(ctx, team.DataRef([]byte("x")), []byte("x"))) // runs gc: too early
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, team.DataRef(stray))); err != nil {
		t.Fatal("pending upload dropped too early")
	}
	h.clock.advance(pendingTTL)
	must(t, a.PutData(ctx, team.DataRef([]byte("y")), []byte("y")))
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, team.DataRef(stray))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending upload not dropped")
	}

	// The hub reloads the same state.
	s2, err := New(Options{DataDir: h.dir})
	must(t, err)
	t2 := s2.teams[a.Key.Team]
	if t2 == nil || t2.head().Seq != 5 || len(t2.Heads) != 1 || !t2.Heads[bundle].Deleted {
		t.Fatalf("reloaded state: %+v", t2)
	}
	// Nothing in the index is a name or content: the sealed names and the
	// team name are opaque, and the data dir holds only ciphertext. (The
	// names are long enough not to turn up in base64 by chance.)
	index, err := os.ReadFile(h.s.store.teamFile(a.Key.Team))
	must(t, err)
	for _, secret := range []string{"codeline", "florian", "beatrice", "cyrille", "git-commit", "commit well", "SKILL.md"} {
		if bytes.Contains(index, []byte(secret)) {
			t.Errorf("index contains %q in plaintext", secret)
		}
	}
	files, _ := os.ReadDir(filepath.Join(h.s.store.teamDir(a.Key.Team), "data"))
	for _, f := range files {
		blob, _ := os.ReadFile(filepath.Join(h.s.store.teamDir(a.Key.Team), "data", f.Name()))
		if bytes.Contains(blob, []byte("skill tar.gz")) {
			t.Error("data blob holds plaintext")
		}
	}
}

func mustErr[T any](_ T, err error) error { return err }

func TestCreateNeedsCode(t *testing.T) {
	h := newTestHub(t, Options{})
	ctx := context.Background()
	a, err := team.NewMemberKey(h.ts.URL, team.NewID())
	must(t, err)
	r1, K, err := team.Found(a, "x", "y")
	must(t, err)
	keys, err := team.SealKeys(r1, K)
	must(t, err)
	req := team.CreateRequest{Roster: r1, Keys: keys}
	wantStatus(t, team.Create(ctx, h.ts.URL, "", req), http.StatusUnauthorized, "no code")
	wantStatus(t, team.Create(ctx, h.ts.URL, "ABCD-EFGH-JKLM", req), http.StatusUnauthorized, "wrong code")
	code, _ := h.s.codes.issue()
	must(t, team.Create(ctx, h.ts.URL, strings.ToLower(code), req)) // case and dashes do not matter
	wantStatus(t, team.Create(ctx, h.ts.URL, code, req), http.StatusUnauthorized, "code used twice")
	code2, _ := h.s.codes.issue()
	wantStatus(t, team.Create(ctx, h.ts.URL, code2, req), http.StatusConflict, "team id taken")
	// A code expires.
	code3, _ := h.s.codes.issue()
	h.clock.advance(codeTTL + time.Second)
	wantStatus(t, team.Create(ctx, h.ts.URL, code3, req), http.StatusUnauthorized, "expired code")

	// A roster that is not a founding roster is refused even with a code.
	b, err := team.NewMemberKey(h.ts.URL, team.NewID())
	must(t, err)
	rb, Kb, err := team.Found(b, "x", "y")
	must(t, err)
	rb.Members[0].Admin = false
	code4, _ := h.s.codes.issue()
	wantStatus(t, team.Create(ctx, h.ts.URL, code4, team.CreateRequest{Roster: rb}), http.StatusUnprocessableEntity, "tampered founding roster")
	// Keys for another epoch are refused.
	rb, Kb, err = team.Found(b, "x", "y")
	must(t, err)
	bad, err := team.SealTeamKey(rb.Team, 7, Kb, rb.Members[0])
	must(t, err)
	code5, _ := h.s.codes.issue()
	wantStatus(t, team.Create(ctx, h.ts.URL, code5, team.CreateRequest{Roster: rb, Keys: []team.SealedKey{bad}}), http.StatusUnprocessableEntity, "key for wrong epoch")

	// An open hub needs no code.
	open := newTestHub(t, Options{Open: true})
	c, err := team.NewMemberKey(open.ts.URL, team.NewID())
	must(t, err)
	rc, _, err := team.Found(c, "x", "y")
	must(t, err)
	must(t, team.Create(ctx, open.ts.URL, "", team.CreateRequest{Roster: rc}))
}

func TestOperatorCode(t *testing.T) {
	h := newTestHub(t, Options{})
	op := httptest.NewServer(h.s.operatorHandler("secret-token"))
	defer op.Close()
	dir := t.TempDir()
	must(t, writeFileAtomic(filepath.Join(dir, "operator.json"), []byte(`{"token":"secret-token","addr":"`+strings.TrimPrefix(op.URL, "http://")+`"}`)))
	reply, err := RequestCode(context.Background(), dir)
	must(t, err)
	if len(reply.Code) != 14 || !h.s.codes.redeem(reply.Code) {
		t.Fatalf("code %q not redeemable", reply.Code)
	}
	// The wrong token gets nothing.
	must(t, writeFileAtomic(filepath.Join(dir, "operator.json"), []byte(`{"token":"nope","addr":"`+strings.TrimPrefix(op.URL, "http://")+`"}`)))
	if _, err := RequestCode(context.Background(), dir); err == nil {
		t.Fatal("wrong operator token accepted")
	}
	if _, err := RequestCode(context.Background(), t.TempDir()); err == nil {
		t.Fatal("no operator.json accepted")
	}
	// Too many wrong codes clear the live ones.
	live, _ := h.s.codes.issue()
	for i := 0; i < maxCodeFailures; i++ {
		h.s.codes.redeem("AAAA-AAAA-AAAA")
	}
	if h.s.codes.redeem(live) {
		t.Fatal("live code survived the failure cap")
	}
}

// raw sends a hand-made signed request, for the auth tests.
func (h *testHub) raw(t *testing.T, k *team.MemberKey, nonce, method, route string, body []byte, sign func(string) string) *http.Response {
	t.Helper()
	uri := "/v1/teams/" + k.Team + route
	req, err := http.NewRequest(method, h.ts.URL+uri, bytes.NewReader(body))
	must(t, err)
	auth := k.SignRequest(nonce, method, uri, body)
	if sign != nil {
		auth = sign(auth)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *testHub) nonce(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(h.ts.URL + "/v1/nonce")
	must(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	_, after, _ := strings.Cut(string(b), `"nonce":"`)
	nonce, _, _ := strings.Cut(after, `"`)
	if nonce == "" {
		t.Fatalf("no nonce in %s", b)
	}
	return nonce
}

func TestAuth(t *testing.T) {
	h := newTestHub(t, Options{})
	ctx := context.Background()
	a, r1, K := h.founder(t)
	b, r2, _ := h.join(t, a, r1, K, "bea")

	// Good.
	if resp := h.raw(t, a.Key, h.nonce(t), "GET", "/bundles", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("good request: %d", resp.StatusCode)
	}
	// Replayed nonce.
	n := h.nonce(t)
	h.raw(t, a.Key, n, "GET", "/bundles", nil, nil)
	if resp := h.raw(t, a.Key, n, "GET", "/bundles", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("replayed nonce: %d", resp.StatusCode)
	}
	// Made-up nonce, expired nonce.
	if resp := h.raw(t, a.Key, "0123456789abcdef0123456789abcdef", "GET", "/bundles", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("unknown nonce: %d", resp.StatusCode)
	}
	n = h.nonce(t)
	h.clock.advance(nonceTTL + time.Second)
	if resp := h.raw(t, a.Key, n, "GET", "/bundles", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("expired nonce: %d", resp.StatusCode)
	}
	// Bad signature: a flipped bit, and a nonce spent by the attempt.
	flip := func(auth string) string {
		last := auth[len(auth)-1]
		if last == '0' {
			return auth[:len(auth)-1] + "1"
		}
		return auth[:len(auth)-1] + "0"
	}
	if resp := h.raw(t, a.Key, h.nonce(t), "GET", "/bundles", nil, flip); resp.StatusCode != 401 {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	// Signature for another route or body.
	n = h.nonce(t)
	req, _ := http.NewRequest("GET", h.ts.URL+"/v1/teams/"+a.Key.Team+"/keys", nil)
	req.Header.Set("Authorization", a.Key.SignRequest(n, "GET", "/v1/teams/"+a.Key.Team+"/bundles", nil))
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("signature for another route: %d", resp.StatusCode)
	}
	// A stranger with a valid-looking key.
	stranger, err := team.NewMemberKey(h.ts.URL, a.Key.Team)
	must(t, err)
	if resp := h.raw(t, stranger, h.nonce(t), "GET", "/bundles", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("stranger: %d", resp.StatusCode)
	}
	// An unknown team looks the same as a non-membership.
	other, err := team.NewMemberKey(h.ts.URL, team.NewID())
	must(t, err)
	if resp := h.raw(t, other, h.nonce(t), "GET", "/bundles", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("unknown team: %d", resp.StatusCode)
	}
	// No Authorization at all, or the fleet-key scheme.
	req, _ = http.NewRequest("GET", h.ts.URL+"/v1/teams/"+a.Key.Team+"/bundles", nil)
	resp, err = http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no auth: %d", resp.StatusCode)
	}

	// Non-admin posting a roster, keys or an invite.
	r3, err := team.SetAdmin(r2, a.Key, b.Key.ID, false) // no change, but a valid next roster signed by a
	must(t, err)
	wantStatus(t, b.PostRoster(ctx, r3, nil), http.StatusForbidden, "non-admin roster")
	wantStatus(t, b.PostKeys(ctx, []team.SealedKey{{}}), http.StatusForbidden, "non-admin keys")
	inv, _ := team.NewInvite(h.ts.URL, a.Key.Team, b.Key.ID)
	wantStatus(t, b.RegisterInvite(ctx, inv, h.clock.now().Add(time.Hour)), http.StatusForbidden, "non-admin invite")
	wantStatus(t, mustErr(b.Joins(ctx)), http.StatusForbidden, "non-admin joins")
	// An admin posting a roster signed by someone else, or not following
	// the chain.
	rb, err := team.SetAdmin(r2, a.Key, b.Key.ID, true)
	must(t, err)
	must(t, a.PostRoster(ctx, rb, nil))
	rbb, err := team.SetAdmin(rb, b.Key, b.Key.ID, true)
	must(t, err)
	wantStatus(t, a.PostRoster(ctx, rbb, nil), http.StatusForbidden, "roster signed by another admin")
	must(t, b.PostRoster(ctx, rbb, nil))
	wantStatus(t, a.PostRoster(ctx, rb, nil), http.StatusConflict, "stale roster")
	forged, err := team.SetAdmin(rbb, a.Key, b.Key.ID, false)
	must(t, err)
	forged.Epoch = 0
	wantStatus(t, a.PostRoster(ctx, forged, nil), http.StatusUnprocessableEntity, "tampered roster")
	// Keys for a non-member or the wrong epoch.
	wantStatus(t, a.PostKeys(ctx, []team.SealedKey{{Team: a.Key.Team, Epoch: 1, Member: stranger.ID, Sealed: []byte("x")}}), http.StatusUnprocessableEntity, "key for non-member")
	wantStatus(t, a.PostKeys(ctx, []team.SealedKey{{Team: a.Key.Team, Epoch: 2, Member: b.Key.ID, Sealed: []byte("x")}}), http.StatusUnprocessableEntity, "key for wrong epoch")
	// Invite expiry out of range.
	wantStatus(t, a.RegisterInvite(ctx, inv, h.clock.now().Add(-time.Hour)), http.StatusUnprocessableEntity, "expired invite")
	wantStatus(t, a.RegisterInvite(ctx, inv, h.clock.now().Add(2*maxInviteTTL)), http.StatusUnprocessableEntity, "invite too long")
}

func TestJoinAuth(t *testing.T) {
	h := newTestHub(t, Options{})
	ctx := context.Background()
	a, _, _ := h.founder(t)
	inv, err := team.NewInvite(h.ts.URL, a.Key.Team, a.Key.ID)
	must(t, err)
	b, err := team.NewMemberKey(h.ts.URL, a.Key.Team)
	must(t, err)
	jr, err := inv.NewJoinRequest(b, "bea")
	must(t, err)
	// Not registered yet.
	wantStatus(t, team.Join(ctx, inv, jr), http.StatusForbidden, "unregistered invite")
	must(t, a.RegisterInvite(ctx, inv, h.clock.now().Add(time.Hour)))
	// Garbage keys with a good credential: refused, invite still live.
	bad := jr
	bad.KEMPub = []byte("short")
	wantStatus(t, team.Join(ctx, inv, bad), http.StatusBadRequest, "bad keys")
	// Expired.
	h.clock.advance(2 * time.Hour)
	wantStatus(t, team.Join(ctx, inv, jr), http.StatusForbidden, "expired invite")
	// Fresh one: works once.
	inv2, err := team.NewInvite(h.ts.URL, a.Key.Team, a.Key.ID)
	must(t, err)
	must(t, a.RegisterInvite(ctx, inv2, h.clock.now().Add(time.Hour)))
	jr2, err := inv2.NewJoinRequest(b, "bea")
	must(t, err)
	must(t, team.Join(ctx, inv2, jr2))
	wantStatus(t, team.Join(ctx, inv2, jr2), http.StatusForbidden, "invite used twice")
	// The founder joining again is refused; a member of another team's
	// invite is refused.
	inv3, err := team.NewInvite(h.ts.URL, a.Key.Team, a.Key.ID)
	must(t, err)
	must(t, a.RegisterInvite(ctx, inv3, h.clock.now().Add(time.Hour)))
	jr3, err := inv3.NewJoinRequest(a.Key, "flo")
	must(t, err)
	wantStatus(t, team.Join(ctx, inv3, jr3), http.StatusConflict, "member joining again")
	// The request is listed with what the hub added.
	joins, err := a.Joins(ctx)
	must(t, err)
	if len(joins.Joins) != 1 || joins.Joins[0].ID != b.ID || joins.Joins[0].Invite != inv2.ID() {
		t.Fatalf("joins = %+v", joins.Joins)
	}
	must(t, a.DeleteJoin(ctx, b.ID))
	wantStatus(t, a.DeleteJoin(ctx, b.ID), http.StatusNotFound, "delete twice")
}

func TestQuotaAndBlobs(t *testing.T) {
	h := newTestHub(t, Options{Quota: 20000})
	ctx := context.Background()
	a, _, K := h.founder(t)
	meta := team.Meta{Kind: "skill", Name: "big"}
	big := bytes.Repeat([]byte("x"), 15000)
	h1, s1, err := team.SealBundle(a.Key, team.NewID(), team.NextVersion(0), 1, K, meta, big)
	must(t, err)
	must(t, a.PutData(ctx, h1.DataRef, s1))
	must(t, a.PutHead(ctx, h1))
	h2, s2, err := team.SealBundle(a.Key, team.NewID(), team.NextVersion(0), 1, K, meta, big)
	must(t, err)
	wantStatus(t, a.PutData(ctx, h2.DataRef, s2), http.StatusInsufficientStorage, "over quota")
	// Replacing the first bundle's data frees the old blob as soon as the
	// new head is in: upload the small new version, publish, then the big
	// one fits again.
	h1b, s1b, err := team.SealBundle(a.Key, h1.ID, team.NextVersion(h1.Version), 1, K, meta, []byte("small"))
	must(t, err)
	must(t, a.PutData(ctx, h1b.DataRef, s1b))
	must(t, a.PutHead(ctx, h1b))
	must(t, a.PutData(ctx, h2.DataRef, s2))
	if _, err := os.Stat(h.s.store.blobPath(a.Key.Team, h1.DataRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replaced blob still stored")
	}
	// Data that does not match its ref, or is empty.
	wantStatus(t, a.PutData(ctx, team.DataRef([]byte("a")), []byte("b")), http.StatusBadRequest, "ref mismatch")
	wantStatus(t, a.PutData(ctx, team.DataRef(nil), nil), http.StatusBadRequest, "empty data")
	// A head whose signature does not match (size changed after signing).
	h2.DataSize++
	wantStatus(t, a.PutHead(ctx, h2), http.StatusUnprocessableEntity, "tampered head")
}

func TestEventsLongPoll(t *testing.T) {
	h := newTestHub(t, Options{})
	ctx := context.Background()
	a, _, K := h.founder(t)
	ev, err := a.Events(ctx, 0)
	must(t, err)
	// A poll with nothing new waits until something happens.
	done := make(chan team.EventList, 1)
	go func() {
		got, err := a.Events(ctx, ev.Next)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	time.Sleep(100 * time.Millisecond)
	hd, sealed, err := team.SealBundle(a.Key, team.NewID(), team.NextVersion(0), 1, K, team.Meta{Kind: "skill"}, []byte("d"))
	must(t, err)
	must(t, a.PutData(ctx, hd.DataRef, sealed))
	must(t, a.PutHead(ctx, hd))
	select {
	case got := <-done:
		if len(got.Events) != 1 || got.Events[0].Kind != team.EventBundle || got.Events[0].Ref != hd.ID {
			t.Fatalf("woken with %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll not woken")
	}
	// A cursor older than the log gets Reset.
	ts := h.s.teams[a.Key.Team]
	h.s.mu.Lock()
	for i := 0; i < maxEvents+5; i++ {
		ts.event(team.Event{Kind: team.EventBundle, Ref: "x"}, h.clock.now())
	}
	h.s.mu.Unlock()
	got, err := a.Events(ctx, 1)
	must(t, err)
	if !got.Reset || got.Next != ts.NextEvent-1 || len(got.Events) != 0 {
		t.Fatalf("stale cursor: %+v", got)
	}
	got, err = a.Events(ctx, got.Next)
	must(t, err)
	if got.Reset || len(got.Events) != 0 {
		t.Fatalf("after reset: %+v", got)
	}
	// A cancelled context ends the wait.
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = a.Events(cctx, got.Next)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancelled poll: %v after %v", err, time.Since(start))
	}
}

func TestRateLimit(t *testing.T) {
	h := newTestHub(t, Options{})
	rateLimit = 5
	defer func() { rateLimit = 100000 }()
	for i := 0; i < 5; i++ {
		resp, err := http.Get(h.ts.URL + "/v1/nonce")
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp, err := http.Get(h.ts.URL + "/v1/nonce")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
	h.clock.advance(rateWindow)
	resp, err = http.Get(h.ts.URL + "/v1/nonce")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("next window: %d", resp.StatusCode)
	}
}

func TestDataDirPermissions(t *testing.T) {
	h := newTestHub(t, Options{})
	a, _, _ := h.founder(t)
	for _, p := range []string{h.dir, filepath.Join(h.dir, "teams"), h.s.store.teamDir(a.Key.Team)} {
		fi, err := os.Stat(p)
		must(t, err)
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %o", p, fi.Mode().Perm())
		}
	}
	fi, err := os.Stat(h.s.store.teamFile(a.Key.Team))
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("team.json: mode %o", fi.Mode().Perm())
	}
}
