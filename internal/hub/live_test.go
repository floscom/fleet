package hub

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"fleet/internal/team"
)

// TestLive runs the team flow against a real hub: create, invite, join,
// publish, rotate, re-wrap. It needs a running hub and a creation code:
//
//	FLEET_HUB_URL=http://127.0.0.1:18443 FLEET_HUB_CODE=$(fleet hub code) go test -run TestLive ./internal/hub/
func TestLive(t *testing.T) {
	url, code := os.Getenv("FLEET_HUB_URL"), os.Getenv("FLEET_HUB_CODE")
	if url == "" {
		t.Skip("FLEET_HUB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Create.
	ak, err := team.NewMemberKey(url, team.NewID())
	must(t, err)
	r1, K, err := team.Found(ak, "live team", "flo")
	must(t, err)
	keys, err := team.SealKeys(r1, K)
	must(t, err)
	must(t, team.Create(ctx, url, code, team.CreateRequest{Roster: r1, Keys: keys}))
	a := team.NewClient(ak)
	t.Logf("created team %s", ak.Team)

	// Invite and join.
	inv, err := team.NewInvite(url, ak.Team, ak.ID)
	must(t, err)
	must(t, a.RegisterInvite(ctx, inv, time.Now().Add(time.Hour)))
	parsed, err := team.ParseInvite(inv.String())
	must(t, err)
	bk, err := team.NewMemberKey(parsed.Hub, parsed.Team)
	must(t, err)
	jr, err := parsed.NewJoinRequest(bk, "bea")
	must(t, err)
	must(t, team.Join(ctx, parsed, jr))
	joins, err := a.Joins(ctx)
	must(t, err)
	if len(joins.Joins) != 1 {
		t.Fatalf("%d joins", len(joins.Joins))
	}
	m, name, err := inv.VerifyJoin(joins.Joins[0])
	must(t, err)
	r2, err := team.AddMember(r1, ak, K, m, name)
	must(t, err)
	sealed, err := team.SealTeamKey(r2.Team, r2.Epoch, K, m)
	must(t, err)
	must(t, a.PostRoster(ctx, r2, []team.SealedKey{sealed}))
	b := team.NewClient(bk)
	rosters, err := b.Rosters(ctx, 0)
	must(t, err)
	anchor, err := team.Anchor(rosters, bk.ID, parsed.Admin)
	must(t, err)
	bHead, err := team.Advance(anchor, rosters)
	must(t, err)
	bKeys, err := b.Keys(ctx)
	must(t, err)
	bK, err := bk.OpenTeamKey(bKeys[0], bHead)
	must(t, err)
	t.Logf("%s joined at seq %d", bk.ID, bHead.Seq)

	// Publish and read.
	data := bytes.Repeat([]byte("skill "), 2000)
	h, blob, err := team.SealBundle(ak, team.NewID(), team.NextVersion(0), r2.Epoch, K, team.Meta{Kind: "skill", Name: "live-skill"}, data)
	must(t, err)
	must(t, a.PutData(ctx, h.DataRef, blob))
	must(t, a.PutHead(ctx, h))
	heads, err := b.Heads(ctx)
	must(t, err)
	must(t, team.VerifyHead(heads[0], bHead))
	got, err := b.Data(ctx, heads[0].DataRef)
	must(t, err)
	meta, pt, err := team.OpenBundle(heads[0], bK, got)
	must(t, err)
	if meta.Name != "live-skill" || !bytes.Equal(pt, data) {
		t.Fatal("b read something else")
	}
	t.Logf("b read %s (%d bytes)", meta.Name, len(pt))

	// Rotate and re-wrap.
	r3, K2, err := team.RotateEpoch(r2, ak, K)
	must(t, err)
	keys3, err := team.SealKeys(r3, K2)
	must(t, err)
	must(t, a.PostRoster(ctx, r3, keys3))
	re, err := team.Rewrap(h, ak, K, team.NextVersion(h.Version), r3.Epoch, K2)
	must(t, err)
	must(t, a.PutHead(ctx, re))
	rosters, err = b.Rosters(ctx, bHead.Seq)
	must(t, err)
	bHead, err = team.Advance(bHead, rosters)
	must(t, err)
	bKeys, err = b.Keys(ctx)
	must(t, err)
	bK2, err := bk.OpenTeamKey(bKeys[0], bHead)
	must(t, err)
	heads, err = b.Heads(ctx)
	must(t, err)
	must(t, team.VerifyHead(heads[0], bHead))
	if _, pt, err := team.OpenBundle(heads[0], bK2, got); err != nil || !bytes.Equal(pt, data) {
		t.Fatalf("after rotation: %v", err)
	}
	ev, err := b.Events(ctx, 0)
	must(t, err)
	t.Logf("rotated to epoch %d; b sees %d events", bHead.Epoch, len(ev.Events))
}
