package team

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Test vectors for the canonical encodings, labels and derivations, so a
// change to any of them is a deliberate protocol change (docs/TEAMS.md,
// "Test vectors"). Everything random (nonces, HPKE) is left out; the
// deterministic parts are pinned: ids, signed byte strings, signatures,
// key checks, invite derivations, request messages.
const (
	vecTeam      = "0123456789abcdef0123456789abcdef"
	vecSignSeedA = "a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	vecKEMSeedA  = "c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf"
	vecSignSeedB = "b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecf"
	vecKEMSeedB  = "d0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeef"
	vecTeamKey   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	vecInviteI   = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	vecNonce     = "404142434445464748494a4b4c4d4e4f"

	vecMemberIDA = "a18112b0b7b4225ff30527e0f7cf7a1e"
	vecMemberIDB = "1ccc5644e1da30f8a091f21364d176f6"
	vecKEMPubA   = "1b1c4e2cb0b8a584299aaaefa9d41d094d9c0bc237955e41aaae894a951c5bde" // SHA-256 of the 1216-byte public key

	vecKeyCheck    = "47b8d28426660eae99ad17c4bb45831743008caf296068a02a39330e04651b2f"
	vecTeamKeyInfo = "00000011666c6565742d7465616d2d6b65792d76310000002030313233343536373839616263646566303132333435363738396162636465660000000000000001000000203163636335363434653164613330663861303931663231333634643137366636"
	vecNameAAD     = "00000012666c6565742d7465616d2d6e616d652d7631000000203031323334353637383961626364656630313233343536373839616263646566000000203163636335363434653164613330663861303931663231333634643137366636"
	vecWrapAAD     = "00000012666c6565742d7465616d2d777261702d76310000002030313233343536373839616263646566303132333435363738396162636465660000002066656463626139383736353433323130666564636261393837363534333231300000018bcfe568000000000000000001"

	vecRosterSigned = "3e31cc1e6537174ef14fcb2e87d2142f30267cf6a4670bcd4b8cf52b2f69d376" // SHA-256 of the signed encoding
	vecRosterSig    = "ca701afb4d8942239cea34d4dd996b5e6125f79e1bdb9a6693902376f8f745743026db9fe85de77594a340887a8a15d4a07c426d467cf9482ff01e379d58bc0c"
	vecHeadSigned   = "b5f66263e6a26decedb9ce6b1c37d4f20472e24f1039b8ef41c2f381c6459952"
	vecHeadSig      = "194115af6a6ac85ff6b48d1234101024db495e801c6448a8d96a750117f565566f82bc9998e1585a528ad246468138187cdc76380daa8cef7026499c91560a0e"

	vecInviteString = "fleet-team:https://hub.example/" + vecTeam + "#ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8." + vecMemberIDA
	vecInviteCred   = "b5WTT5Dxtg0TNG6CjrJ6C5XM1HzAjzNiZA-87ynDpOM"
	vecInviteID     = "8d7c9441ec7ef38f1488abed29646d559991b822dd162374849745a519e0f3fd"
	vecJoinProof    = "a5eb9d8acdd7c564cf48e47e05734896c1c50d0eb51536ecaa5d4c8631392f37"

	vecRequestMessage = "666c6565742d7465616d2d6d656d6265722d76310a30313233343536373839616263646566303132333435363738396162636465660a343034313432" +
		"34333434343534363437343834393461346234633464346534660a5055540a2f76312f7465616d732f30313233343536373839616263646566303132" +
		"333435363738396162636465662f62756e646c65732f66656463626139383736353433323130666564636261393837363534333231300a34343133366661333535623336373861313134366164313666376538363439653934666234666332316665373765383331306330363066363163616166663861" // hex of the signed byte string
	vecRequestSig = "10bfd652db2d96d31e68987b0b699f3c1e102d1ec0eba1618face740005605eab39faca47dd3b36b88f7e9af0480260927b730bbba5870e27705ffd6155dc604"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// vectorKey derives a member key from fixed seeds.
func vectorKey(t *testing.T, signSeed, kemSeed string) *MemberKey {
	t.Helper()
	sign := ed25519.NewKeyFromSeed(unhex(t, signSeed))
	k, err := kem().DeriveKeyPair(unhex(t, kemSeed))
	if err != nil {
		t.Fatal(err)
	}
	return &MemberKey{Hub: "https://hub.example", Team: vecTeam, ID: MemberID(sign.Public().(ed25519.PublicKey)), Sign: sign, KEM: k}
}

func TestProtocolVectors(t *testing.T) {
	a, b := vectorKey(t, vecSignSeedA, vecKEMSeedA), vectorKey(t, vecSignSeedB, vecKEMSeedB)
	K := unhex(t, vecTeamKey)

	// A two-member roster with fixed (made-up) sealed names and key check.
	ma, mb := a.Public(), b.Public()
	ma.Admin, ma.NameSealed = true, []byte("sealed-name-a")
	mb.NameSealed = []byte("sealed-name-b")
	members := []Member{ma, mb}
	if ma.ID > mb.ID {
		members = []Member{mb, ma}
	}
	r := &Roster{Team: vecTeam, Seq: 2, Members: members, Epoch: 1, KeyCheck: KeyCheck(vecTeam, 1, K),
		TeamNameSealed: []byte("sealed-team-name"), PrevHash: sha256Sum([]byte("prev")), Signer: a.ID}
	r.Sig = ed25519.Sign(a.Sign, r.signed())
	if err := VerifyAnchor(r, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	h := &Head{Team: vecTeam, ID: "fedcba9876543210fedcba9876543210", Owner: a.ID, Version: 1700000000000, Epoch: 1,
		KeyWrap: []byte("wrapped-key"), MetaSealed: []byte("sealed-meta"), DataRef: hex.EncodeToString(sha256Sum([]byte("data"))), DataSize: 4}
	h.Sig = ed25519.Sign(a.Sign, h.signed())
	if err := VerifyHead(h, r); err != nil {
		t.Fatal(err)
	}

	inv := &Invite{Hub: "https://hub.example", Team: vecTeam, Secret: unhex(t, vecInviteI), Admin: a.ID}
	req := RequestMessage(vecTeam, vecNonce, "PUT", "/v1/teams/"+vecTeam+"/bundles/"+h.ID, sha256Sum([]byte("{}")))

	checks := []struct{ name, got, want string }{
		{"member id a", a.ID, vecMemberIDA},
		{"member id b", b.ID, vecMemberIDB},
		{"kem public key a", hex.EncodeToString(sha256Sum(ma.KEMPub)), vecKEMPubA},
		{"key check", hex.EncodeToString(r.KeyCheck), vecKeyCheck},
		{"team key info", hex.EncodeToString(teamKeyInfo(vecTeam, 1, b.ID)), vecTeamKeyInfo},
		{"name aad", hex.EncodeToString(nameAAD(vecTeam, b.ID)), vecNameAAD},
		{"wrap aad", hex.EncodeToString(wrapAAD(vecTeam, h.ID, h.Version, h.Epoch)), vecWrapAAD},
		{"roster signed", hex.EncodeToString(sha256Sum(r.signed())), vecRosterSigned},
		{"roster sig", hex.EncodeToString(r.Sig), vecRosterSig},
		{"head signed", hex.EncodeToString(sha256Sum(h.signed())), vecHeadSigned},
		{"head sig", hex.EncodeToString(h.Sig), vecHeadSig},
		{"invite string", inv.String(), vecInviteString},
		{"invite credential", inv.Credential(), vecInviteCred},
		{"invite id", inv.ID(), vecInviteID},
		{"join proof", hex.EncodeToString(inv.proof(mb)), vecJoinProof},
		{"request message", hex.EncodeToString(req), vecRequestMessage},
		{"request sig", hex.EncodeToString(ed25519.Sign(a.Sign, req)), vecRequestSig},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
	if p, err := ParseInvite(vecInviteString); err != nil || p.ID() != inv.ID() {
		t.Errorf("ParseInvite(vector) = %v, %v", p, err)
	}
	// The roster hash is the hash of the signed encoding.
	if hex.EncodeToString(r.Hash()) != vecRosterSigned {
		t.Error("Hash() is not SHA-256(signed)")
	}
}
