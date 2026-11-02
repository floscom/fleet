package team

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const testHub = "https://hub.test"

// newKey makes a member key for team t (a fresh team when "").
func newKey(t *testing.T, team string) *MemberKey {
	t.Helper()
	if team == "" {
		team = NewID()
	}
	k, err := NewMemberKey(testHub, team)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// founded is a team of one.
type founded struct {
	a  *MemberKey
	r1 *Roster
	K  []byte
}

func found(t *testing.T) founded {
	t.Helper()
	a := newKey(t, "")
	r1, K, err := Found(a, "codeline", "flo")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFounding(r1); err != nil {
		t.Fatal(err)
	}
	return founded{a, r1, K}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantVerifyErr(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted", what)
	}
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("%s: %v is not an ErrVerify", what, err)
	}
}

func TestMemberKeyFile(t *testing.T) {
	k := newKey(t, "")
	path := filepath.Join(t.TempDir(), "team.key")
	must(t, k.Save(path))
	l, err := LoadMemberKey(path)
	must(t, err)
	if l.ID != k.ID || l.Team != k.Team || l.Hub != k.Hub {
		t.Fatal("loaded key differs")
	}
	if !bytes.Equal(l.Public().KEMPub, k.Public().KEMPub) || !bytes.Equal(l.Public().SignPub, k.Public().SignPub) {
		t.Fatal("loaded public keys differ")
	}
	// The loaded key opens what was sealed to the original.
	r := &Roster{Team: k.Team, Epoch: 3}
	K := NewTeamKey()
	r.KeyCheck = KeyCheck(k.Team, 3, K)
	s, err := SealTeamKey(k.Team, 3, K, k.Public())
	must(t, err)
	got, err := l.OpenTeamKey(s, r)
	must(t, err)
	if !bytes.Equal(got, K) {
		t.Fatal("opened key differs")
	}
}

func TestChain(t *testing.T) {
	f := found(t)
	a, r1, K1 := f.a, f.r1, f.K
	b, c := newKey(t, a.Team), newKey(t, a.Team)

	// a invites b.
	r2, err := AddMember(r1, a, K1, b.Public(), "bea")
	must(t, err)
	must(t, VerifyNext(r1, r2))
	keys, err := SealKeys(r2, K1)
	must(t, err)
	if len(keys) != 2 {
		t.Fatalf("sealed %d keys, want 2", len(keys))
	}

	// b anchors at r2, signed by a (the inviting admin), and gets K1.
	anchor, err := Anchor([]*Roster{r2, r1}, b.ID, a.ID)
	must(t, err)
	if anchor.Seq != 2 {
		t.Fatalf("anchor seq %d, want 2", anchor.Seq)
	}
	var bK []byte
	for _, s := range keys {
		if s.Member == b.ID {
			bK, err = b.OpenTeamKey(s, anchor)
			must(t, err)
		}
	}
	if !bytes.Equal(bK, K1) {
		t.Fatal("b got a different team key")
	}
	if name, err := OpenName(bK, a.Team, a.ID, anchor.Members[idx(anchor, a.ID)].NameSealed); err != nil || name != "flo" {
		t.Fatalf("a's name = %q, %v", name, err)
	}
	if name, err := OpenTeamName(bK, a.Team, anchor.TeamNameSealed); err != nil || name != "codeline" {
		t.Fatalf("team name = %q, %v", name, err)
	}

	// b is not an admin yet and cannot add c.
	if _, err := AddMember(r2, b, K1, c.Public(), "cy"); err == nil {
		t.Fatal("non-admin built a roster")
	}
	r3, err := SetAdmin(r2, a, b.ID, true)
	must(t, err)
	r4, err := AddMember(r3, b, K1, c.Public(), "cy")
	must(t, err)
	if r4.Signer != b.ID {
		t.Fatal("r4 not signed by b")
	}

	// a removes b: new epoch, names re-sealed.
	r5, K2, err := RemoveMember(r4, a, K1, b.ID)
	must(t, err)
	if r5.Epoch != 2 || bytes.Equal(K2, K1) {
		t.Fatal("no new epoch")
	}
	if _, ok := r5.Member(b.ID); ok {
		t.Fatal("b still a member")
	}
	if name, err := OpenName(K2, a.Team, c.ID, r5.Members[idx(r5, c.ID)].NameSealed); err != nil || name != "cy" {
		t.Fatalf("c's name after rotation = %q, %v", name, err)
	}

	// c anchors at r4 (signed by b, who invited) and advances to r5.
	all := []*Roster{r1, r2, r3, r4, r5}
	cAnchor, err := Anchor(all, c.ID, b.ID)
	must(t, err)
	head, err := Advance(cAnchor, all)
	must(t, err)
	if head.Seq != 5 {
		t.Fatalf("head seq %d, want 5", head.Seq)
	}
	// Advancing again with the same rosters is a no-op (no rollback).
	head2, err := Advance(head, all)
	must(t, err)
	if head2 != head {
		t.Fatal("advance went somewhere")
	}
	// Anchoring with the wrong inviter fails: a did not sign r4.
	if _, err := Anchor(all, c.ID, a.ID); !errors.Is(err, ErrVerify) {
		t.Fatalf("wrong inviter accepted: %v", err)
	}
	// b, removed, opens nothing new: K2 is not sealed to them.
	keys5, err := SealKeys(r5, K2)
	must(t, err)
	for _, s := range keys5 {
		if s.Member == b.ID {
			t.Fatal("K2 sealed to the removed member")
		}
	}
	// RotateEpoch without a removal.
	r6, K3, err := RotateEpoch(r5, a, K2)
	must(t, err)
	if r6.Epoch != 3 || len(r6.Members) != 2 || bytes.Equal(K3, K2) {
		t.Fatal("rotation went wrong")
	}
	must(t, VerifyNext(r5, r6))
	// Demoting the last admin is refused.
	if _, err := SetAdmin(r6, a, a.ID, false); err == nil {
		t.Fatal("last admin demoted")
	}
}

func idx(r *Roster, id string) int {
	for i, m := range r.Members {
		if m.ID == id {
			return i
		}
	}
	return -1
}

func TestVerifyNextRejects(t *testing.T) {
	f := found(t)
	a, r1, K1 := f.a, f.r1, f.K
	b := newKey(t, a.Team)
	r2, err := AddMember(r1, a, K1, b.Public(), "bea")
	must(t, err)

	// Tamper with every signed field of r2.
	mutations := map[string]func(r *Roster){
		"team":       func(r *Roster) { r.Team = NewID() },
		"seq":        func(r *Roster) { r.Seq++ },
		"epoch":      func(r *Roster) { r.Epoch++ },
		"key check":  func(r *Roster) { r.KeyCheck[0] ^= 1 },
		"team name":  func(r *Roster) { r.TeamNameSealed[3] ^= 1 },
		"prev hash":  func(r *Roster) { r.PrevHash[0] ^= 1 },
		"signer":     func(r *Roster) { r.Signer = b.ID },
		"sig":        func(r *Roster) { r.Sig[5] ^= 1 },
		"admin flag": func(r *Roster) { r.Members[idx(r, b.ID)].Admin = true },
		"name":       func(r *Roster) { r.Members[idx(r, b.ID)].NameSealed[2] ^= 1 },
		"kem key":    func(r *Roster) { r.Members[idx(r, b.ID)].KEMPub[7] ^= 1 },
		"sign key":   func(r *Roster) { r.Members[idx(r, b.ID)].SignPub[7] ^= 1 },
		"member id":  func(r *Roster) { r.Members[idx(r, b.ID)].ID = NewID() },
		"drop":       func(r *Roster) { r.Members = r.Members[:1] },
		"order":      func(r *Roster) { r.Members[0], r.Members[1] = r.Members[1], r.Members[0] },
	}
	for name, mutate := range mutations {
		m := cloneRoster(r2)
		mutate(m)
		wantVerifyErr(t, VerifyNext(r1, m), "mutated "+name)
	}
	must(t, VerifyNext(r1, r2)) // the original still passes

	// A roster signed by a non-admin: b builds r3 by hand with b's key.
	r3 := cloneRoster(r2)
	r3.Seq, r3.PrevHash = 3, r2.Hash()
	must(t, r3.sign(b))
	wantVerifyErr(t, VerifyNext(r2, r3), "signer not admin")

	// A removal without a new epoch.
	r3 = cloneRoster(r2)
	r3.Seq, r3.PrevHash = 3, r2.Hash()
	r3.Members = []Member{r2.Members[idx(r2, a.ID)]}
	must(t, r3.sign(a))
	wantVerifyErr(t, VerifyNext(r2, r3), "removal without epoch")

	// Epoch going down.
	r3 = cloneRoster(r2)
	r3.Seq, r3.PrevHash, r3.Epoch = 3, r2.Hash(), 0
	must(t, r3.sign(a))
	wantVerifyErr(t, VerifyNext(r2, r3), "epoch down")

	// Rollback: Advance from r2 ignores r1 and a forged lower seq.
	head, err := Advance(r2, []*Roster{r1})
	must(t, err)
	if head != r2 {
		t.Fatal("rolled back")
	}
	// A gap: seq 4 without seq 3.
	r4, err := SetAdmin(r2, a, b.ID, true)
	must(t, err)
	r4.Seq = 4
	must(t, r4.sign(a))
	wantVerifyErr(t, VerifyNext(r2, r4), "seq gap")

	// A founding roster that is not: two members, or signed by a non-member.
	wantVerifyErr(t, VerifyFounding(r2), "founding with two members")
	r1b := cloneRoster(r1)
	r1b.Members[0].Admin = false
	r1b.Sig = ed25519.Sign(a.Sign, r1b.signed())
	if err := VerifyFounding(r1b); !errors.Is(err, ErrVerify) {
		t.Fatalf("founding without admin accepted: %v", err)
	}
}

// TestFabricatedChain is the attack the anchor rule exists for: the hub
// founds a team of its own, adds the real admin's public key to it, then
// adds the joiner, all signed by its own founder key. The joiner must not
// take that chain, because the roster that first lists them is not signed
// by the admin who invited them.
func TestFabricatedChain(t *testing.T) {
	team := NewID()
	hub := newKey(t, team) // the hub's own "founder"
	real := newKey(t, team)
	joiner := newKey(t, team)

	h1, hK, err := Found(hub, "codeline", "flo")
	must(t, err)
	h2, err := AddMember(h1, hub, hK, real.Public(), "flo")
	must(t, err)
	h3, err := AddMember(h2, hub, hK, joiner.Public(), "bea")
	must(t, err)
	chain := []*Roster{h1, h2, h3}
	// The chain itself is internally valid...
	must(t, VerifyFounding(h1))
	head, err := Advance(h1, chain)
	must(t, err)
	if head != h3 {
		t.Fatal("chain did not advance")
	}
	// ...but the joiner, invited by real, refuses it.
	if _, err := Anchor(chain, joiner.ID, real.ID); !errors.Is(err, ErrVerify) {
		t.Fatalf("fabricated chain accepted: %v", err)
	}
	// And the hub cannot fix that by making real an admin in its chain: the
	// anchor must be signed by real's key, which the hub does not have.
	h3b, err := SetAdmin(h2, hub, real.ID, true)
	must(t, err)
	h4, err := AddMember(h3b, hub, hK, joiner.Public(), "bea")
	must(t, err)
	if _, err := Anchor([]*Roster{h1, h2, h3b, h4}, joiner.ID, real.ID); !errors.Is(err, ErrVerify) {
		t.Fatalf("fabricated chain with real as admin accepted: %v", err)
	}
	// A forged signature does not help either.
	h4.Signer = real.ID
	if _, err := Anchor([]*Roster{h1, h2, h3b, h4}, joiner.ID, real.ID); !errors.Is(err, ErrVerify) {
		t.Fatalf("forged signer accepted: %v", err)
	}

	// The honest case for contrast: real founds and adds the joiner.
	r1, rK, err := Found(real, "codeline", "flo")
	must(t, err)
	r2, err := AddMember(r1, real, rK, joiner.Public(), "bea")
	must(t, err)
	if _, err := Anchor([]*Roster{r1, r2}, joiner.ID, real.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTeamKeyRejects(t *testing.T) {
	f := found(t)
	a, r1, K := f.a, f.r1, f.K
	b := newKey(t, a.Team)
	s, err := SealTeamKey(a.Team, 1, K, a.Public())
	must(t, err)
	if _, err := a.OpenTeamKey(s, r1); err != nil {
		t.Fatal(err)
	}
	// Someone else's key does not open it.
	if _, err := b.OpenTeamKey(s, r1); err == nil {
		t.Fatal("opened with another member's key")
	}
	// A flipped bit anywhere fails.
	for i := 0; i < len(s.Sealed); i += len(s.Sealed) / 16 {
		m := s
		m.Sealed = bytes.Clone(s.Sealed)
		m.Sealed[i] ^= 1
		wantVerifyErr(t, mustErr(a.OpenTeamKey(m, r1)), "tampered sealed key")
	}
	// The wrong epoch or team does not open.
	m := s
	m.Epoch = 2
	r2 := cloneRoster(r1)
	r2.Epoch = 2
	wantVerifyErr(t, mustErr(a.OpenTeamKey(m, r2)), "wrong epoch")
	// A hub-made key: valid HPKE, but not the key the roster commits to.
	fake, err := SealTeamKey(a.Team, 1, NewTeamKey(), a.Public())
	must(t, err)
	wantVerifyErr(t, mustErr(a.OpenTeamKey(fake, r1)), "key not matching roster")
}

func mustErr[T any](_ T, err error) error { return err }

func TestBundle(t *testing.T) {
	f := found(t)
	a, r1, K := f.a, f.r1, f.K
	b := newKey(t, a.Team)
	r2, err := AddMember(r1, a, K, b.Public(), "bea")
	must(t, err)
	meta := Meta{Kind: "skill", Name: "git-commit", Description: "commit well", Hash: "abc", Size: 1234, Files: []string{"SKILL.md"}}
	data := bytes.Repeat([]byte("tar.gz bytes "), 1000)
	id := NewID()
	h, sealed, err := SealBundle(a, id, NextVersion(0), 1, K, meta, data)
	must(t, err)
	must(t, VerifyHead(h, r2))
	must(t, VerifyData(h, sealed))
	gotMeta, gotData, err := OpenBundle(h, K, sealed)
	must(t, err)
	if gotMeta.Name != "git-commit" || gotMeta.Files[0] != "SKILL.md" || !bytes.Equal(gotData, data) {
		t.Fatal("opened bundle differs")
	}
	if m, err := OpenMeta(h, K); err != nil || m.Description != "commit well" {
		t.Fatalf("OpenMeta = %+v, %v", m, err)
	}

	// Tamper with the head: each signed field.
	mutations := map[string]func(h *Head){
		"team":     func(h *Head) { h.Team = otherID(h.Team) },
		"id":       func(h *Head) { h.ID = NewID() },
		"owner":    func(h *Head) { h.Owner = b.ID },
		"version":  func(h *Head) { h.Version++ },
		"epoch":    func(h *Head) { h.Epoch = 0 },
		"key wrap": func(h *Head) { h.KeyWrap[1] ^= 1 },
		"meta":     func(h *Head) { h.MetaSealed[1] ^= 1 },
		"data ref": func(h *Head) { h.DataRef = DataRef([]byte("x")) },
		"size":     func(h *Head) { h.DataSize++ },
		"deleted":  func(h *Head) { h.Deleted = true },
		"sig":      func(h *Head) { h.Sig[0] ^= 1 },
	}
	for name, mutate := range mutations {
		m := cloneHead(h)
		mutate(m)
		wantVerifyErr(t, VerifyHead(m, r2), "mutated "+name)
	}
	// Sealed parts tampered after signing fail to open even with a re-sign.
	m := cloneHead(h)
	m.KeyWrap[len(m.KeyWrap)-1] ^= 1
	must(t, m.sign(a))
	wantVerifyErr(t, mustErr(OpenMeta(m, K)), "tampered key wrap")
	m = cloneHead(h)
	m.MetaSealed[len(m.MetaSealed)-1] ^= 1
	must(t, m.sign(a))
	wantVerifyErr(t, mustErr(OpenMeta(m, K)), "tampered meta")
	bad := bytes.Clone(sealed)
	bad[100] ^= 1
	wantVerifyErr(t, VerifyData(h, bad), "tampered data")
	m = cloneHead(h)
	m.DataRef, m.DataSize = DataRef(bad), int64(len(bad)) // hub re-signs? it cannot, but say the owner did
	must(t, m.sign(a))
	if _, _, err := OpenBundle(m, K, bad); !errors.Is(err, ErrVerify) {
		t.Fatalf("tampered data opened: %v", err)
	}
	// The wrong team key.
	wantVerifyErr(t, mustErr(OpenMeta(h, NewTeamKey())), "wrong team key")
	// A head signed by b claiming a as owner, or b publishing as themselves
	// while not a member of the roster checked against.
	m = cloneHead(h)
	must(t, m.sign(b))
	wantVerifyErr(t, VerifyHead(m, r2), "signed by another member")
	hb, _, err := SealBundle(b, NewID(), NextVersion(0), 1, K, meta, data)
	must(t, err)
	must(t, VerifyHead(hb, r2))
	wantVerifyErr(t, VerifyHead(hb, r1), "owner not in roster")

	// Re-wrap for a new epoch: same data, opens with the new key only.
	r3, K2, err := RemoveMember(r2, a, K, b.ID)
	must(t, err)
	h2, err := Rewrap(h, a, K, NextVersion(h.Version), r3.Epoch, K2)
	must(t, err)
	must(t, VerifyHead(h2, r3))
	if h2.DataRef != h.DataRef || h2.Version <= h.Version || h2.Epoch != 2 {
		t.Fatal("rewrap changed the data or did not bump version/epoch")
	}
	if _, gotData, err := OpenBundle(h2, K2, sealed); err != nil || !bytes.Equal(gotData, data) {
		t.Fatalf("rewrapped bundle: %v", err)
	}
	wantVerifyErr(t, mustErr(OpenMeta(h2, K)), "old key on rewrapped head")
	if _, err := Rewrap(h, b, K, NextVersion(h.Version), 2, K2); err == nil {
		t.Fatal("b rewrapped a's bundle")
	}
	// Tombstone.
	tomb, err := Tombstone(h2, a, NextVersion(h2.Version))
	must(t, err)
	must(t, VerifyHead(tomb, r3))
	if _, err := OpenMeta(tomb, K2); err == nil {
		t.Fatal("opened a tombstone")
	}
	// A version of 0 or a wrong epoch is refused.
	if _, _, err := SealBundle(a, id, 0, 1, K, meta, data); err == nil {
		t.Fatal("version 0 accepted")
	}
	m = cloneHead(h2)
	m.Epoch = 9
	must(t, m.sign(a))
	wantVerifyErr(t, VerifyHead(m, r3), "epoch from the future")
}

func TestInvite(t *testing.T) {
	f := found(t)
	a := f.a
	inv, err := NewInvite(testHub+"/", a.Team, a.ID)
	must(t, err)
	s := inv.String()
	if !strings.HasPrefix(s, "fleet-team:"+testHub+"/"+a.Team+"#") || !strings.HasSuffix(s, "."+a.ID) {
		t.Fatalf("invite string %q", s)
	}
	p, err := ParseInvite(" " + s + "\n")
	must(t, err)
	if p.Hub != testHub || p.Team != a.Team || p.Admin != a.ID || !bytes.Equal(p.Secret, inv.Secret) {
		t.Fatalf("parsed %+v", p)
	}
	if p.ID() != inv.ID() || p.Credential() != inv.Credential() {
		t.Fatal("derived values differ after parsing")
	}
	cred, ok := ParseCredential(inv.Credential())
	if !ok || InviteID(cred) != inv.ID() {
		t.Fatal("credential does not hash to the invite id")
	}
	// A hub with a path keeps it.
	p2, err := ParseInvite("fleet-team:https://x.example/fleet/" + a.Team + "#" + s[strings.Index(s, "#")+1:])
	must(t, err)
	if p2.Hub != "https://x.example/fleet" {
		t.Fatalf("hub with path = %q", p2.Hub)
	}
	for _, bad := range []string{"", "fleet-team:", "fleet-team:https://h/" + a.Team, "fleet-team:https://h/" + a.Team + "#abc." + a.ID,
		"fleet-team:https://h/" + a.Team + "#" + strings.Repeat("A", 43), "fleet-team:ftp://h/" + a.Team + "#" + strings.Repeat("A", 43) + "." + a.ID,
		"fleet-team:https://h/nope#" + strings.Repeat("A", 43) + "." + a.ID} {
		if _, err := ParseInvite(bad); err == nil {
			t.Errorf("ParseInvite(%q) accepted", bad)
		}
	}

	// Join.
	b := newKey(t, a.Team)
	j, err := inv.NewJoinRequest(b, "bea")
	must(t, err)
	m, name, err := inv.VerifyJoin(j)
	must(t, err)
	if m.ID != b.ID || name != "bea" || !bytes.Equal(m.KEMPub, b.Public().KEMPub) {
		t.Fatalf("verified join = %s %q", m.ID, name)
	}
	// A hub substituting keys, or tampering anywhere, fails.
	c := newKey(t, a.Team)
	mutations := map[string]func(j *JoinRequest){
		"sign key": func(j *JoinRequest) { j.SignPub = c.Public().SignPub },
		"kem key":  func(j *JoinRequest) { j.KEMPub = c.Public().KEMPub },
		"proof":    func(j *JoinRequest) { j.Proof[0] ^= 1 },
		"name":     func(j *JoinRequest) { j.NameSealed[0] ^= 1 },
	}
	for what, mutate := range mutations {
		mj := j
		mj.SignPub, mj.KEMPub, mj.Proof, mj.NameSealed = bytes.Clone(j.SignPub), bytes.Clone(j.KEMPub), bytes.Clone(j.Proof), bytes.Clone(j.NameSealed)
		mutate(&mj)
		if _, _, err := inv.VerifyJoin(mj); !errors.Is(err, ErrVerify) {
			t.Errorf("mutated %s: %v", what, err)
		}
	}
	// Another invite (same team, other secret) does not verify it.
	inv2, err := NewInvite(testHub, a.Team, a.ID)
	must(t, err)
	if _, _, err := inv2.VerifyJoin(j); !errors.Is(err, ErrVerify) {
		t.Fatalf("other invite verified the join: %v", err)
	}
	if _, err := inv.NewJoinRequest(newKey(t, ""), "x"); err == nil {
		t.Fatal("join request for another team")
	}
}

func TestRequestSignature(t *testing.T) {
	a := newKey(t, "")
	body := []byte(`{"x":1}`)
	uri := "/v1/teams/" + a.Team + "/bundles/abc"
	hdr := a.SignRequest("n0nce", "PUT", uri, body)
	id, nonce, sig, ok := ParseMemberAuth(hdr)
	if !ok || id != a.ID || nonce != "n0nce" {
		t.Fatalf("ParseMemberAuth(%q) = %q %q %v", hdr, id, nonce, ok)
	}
	sum := sha256Sum(body)
	pub := a.Public().SignPub
	if !VerifyRequest(pub, a.Team, nonce, "PUT", uri, sum, sig) {
		t.Fatal("signature does not verify")
	}
	for what, ok := range map[string]bool{
		"other team":  VerifyRequest(pub, NewID(), nonce, "PUT", uri, sum, sig),
		"other nonce": VerifyRequest(pub, a.Team, "x", "PUT", uri, sum, sig),
		"other verb":  VerifyRequest(pub, a.Team, nonce, "POST", uri, sum, sig),
		"other uri":   VerifyRequest(pub, a.Team, nonce, "PUT", uri+"?x", sum, sig),
		"other body":  VerifyRequest(pub, a.Team, nonce, "PUT", uri, sha256Sum([]byte("{}")), sig),
	} {
		if ok {
			t.Errorf("%s: verified", what)
		}
	}
	for _, bad := range []string{"", "Fleet-Key x.y", MemberScheme + "a.b.c", MemberScheme + a.ID + ".n.zz", MemberScheme + a.ID + "..00"} {
		if _, _, _, ok := ParseMemberAuth(bad); ok {
			t.Errorf("ParseMemberAuth(%q) ok", bad)
		}
	}
}

// otherID is a valid id that differs from id.
func otherID(id string) string {
	if id[31] == '0' {
		return id[:31] + "1"
	}
	return id[:31] + "0"
}

func cloneRoster(r *Roster) *Roster {
	n := *r
	n.Members = make([]Member, len(r.Members))
	for i, m := range r.Members {
		m.SignPub, m.KEMPub, m.NameSealed = bytes.Clone(m.SignPub), bytes.Clone(m.KEMPub), bytes.Clone(m.NameSealed)
		n.Members[i] = m
	}
	n.KeyCheck, n.TeamNameSealed, n.PrevHash, n.Sig = bytes.Clone(r.KeyCheck), bytes.Clone(r.TeamNameSealed), bytes.Clone(r.PrevHash), bytes.Clone(r.Sig)
	return &n
}

func cloneHead(h *Head) *Head {
	n := *h
	n.KeyWrap, n.MetaSealed, n.Sig = bytes.Clone(h.KeyWrap), bytes.Clone(h.MetaSealed), bytes.Clone(h.Sig)
	return &n
}
