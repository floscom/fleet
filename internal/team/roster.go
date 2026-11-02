package team

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxMembers bounds a roster: a team is a handful of people, and the hub
// has to store a KEM public key per member.
const MaxMembers = 256

// Member is one entry of a roster.
type Member struct {
	ID      string `json:"id"`
	SignPub []byte `json:"sign_pub"`
	KEMPub  []byte `json:"kem_pub"`
	Admin   bool   `json:"admin"`
	// NameSealed is the display name, sealed with the team key of the
	// roster's epoch (SealName).
	NameSealed []byte `json:"name_sealed,omitempty"`
}

// Roster is the member list of a team at one point in time, signed by an
// admin. Members are sorted by id.
type Roster struct {
	Team    string   `json:"team"`
	Seq     uint64   `json:"seq"`
	Members []Member `json:"members"`
	// Epoch is the current team key epoch; KeyCheck commits to that key
	// (see KeyCheck).
	Epoch    uint64 `json:"epoch"`
	KeyCheck []byte `json:"key_check"`
	// TeamNameSealed is the team's display name, sealed with the team key.
	TeamNameSealed []byte `json:"team_name_sealed,omitempty"`
	// PrevHash is Hash() of the roster before this one; empty at seq 1.
	PrevHash []byte `json:"prev_hash,omitempty"`
	// Signer is the admin who signed, Sig their Ed25519 signature over the
	// canonical encoding of all other fields.
	Signer string `json:"signer"`
	Sig    []byte `json:"sig"`
}

// signed is the canonical encoding the signature covers.
func (r *Roster) signed() []byte {
	c := newCanon(labelRoster)
	c.str(r.Team)
	c.u64(r.Seq)
	c.u32(uint32(len(r.Members)))
	for _, m := range r.Members {
		c.str(m.ID)
		c.bytes(m.SignPub)
		c.bytes(m.KEMPub)
		c.boolean(m.Admin)
		c.bytes(m.NameSealed)
	}
	c.u64(r.Epoch)
	c.bytes(r.KeyCheck)
	c.bytes(r.TeamNameSealed)
	c.bytes(r.PrevHash)
	c.str(r.Signer)
	return c.buf
}

// Hash identifies the roster for the next one's PrevHash: SHA-256 of the
// signed encoding.
func (r *Roster) Hash() []byte {
	s := sha256.Sum256(r.signed())
	return s[:]
}

// Member returns the member with id.
func (r *Roster) Member(id string) (Member, bool) {
	i, ok := slices.BinarySearchFunc(r.Members, id, func(m Member, id string) int { return strings.Compare(m.ID, id) })
	if !ok {
		return Member{}, false
	}
	return r.Members[i], true
}

// IsAdmin reports whether id is an admin of r.
func (r *Roster) IsAdmin(id string) bool {
	m, ok := r.Member(id)
	return ok && m.Admin
}

// Admins lists the admins' ids.
func (r *Roster) Admins() []string {
	var out []string
	for _, m := range r.Members {
		if m.Admin {
			out = append(out, m.ID)
		}
	}
	return out
}

// checkShape checks everything about r that needs no other roster: ids,
// keys, sorting, sizes. The signature is not checked (the key to check it
// with depends on the context).
func (r *Roster) checkShape() error {
	if !ValidID(r.Team) {
		return verifyErr("roster: bad team id")
	}
	if r.Seq == 0 {
		return verifyErr("roster: seq 0")
	}
	if len(r.Members) == 0 || len(r.Members) > MaxMembers {
		return verifyErr("roster: bad member count")
	}
	admins := 0
	for i, m := range r.Members {
		if i > 0 && r.Members[i-1].ID >= m.ID {
			return verifyErr("roster: members not sorted by id")
		}
		if len(m.SignPub) != ed25519.PublicKeySize || MemberID(m.SignPub) != m.ID {
			return verifyErr("roster: member id does not match its key")
		}
		if _, err := kemPublic(m.KEMPub); err != nil {
			return err
		}
		if m.Admin {
			admins++
		}
	}
	if admins == 0 {
		return verifyErr("roster: no admin")
	}
	if len(r.KeyCheck) != sha256.Size {
		return verifyErr("roster: bad key check")
	}
	if len(r.PrevHash) != 0 && len(r.PrevHash) != sha256.Size {
		return verifyErr("roster: bad prev hash")
	}
	if len(r.Sig) != ed25519.SignatureSize {
		return verifyErr("roster: bad signature size")
	}
	return nil
}

// verifySig checks r's signature with the signer's key as found in from
// (the roster that establishes the signer's key: r itself for an anchor,
// the previous roster otherwise).
func (r *Roster) verifySig(from *Roster) error {
	m, ok := from.Member(r.Signer)
	if !ok || !m.Admin {
		return verifyErr("roster: signer is not an admin")
	}
	if !ed25519.Verify(m.SignPub, r.signed(), r.Sig) {
		return verifyErr("roster: bad signature")
	}
	return nil
}

// VerifyFounding checks a seq 1 roster: one member, who is the admin and
// signed it. It is the founder's own anchor, and what a hub accepts when a
// team is created.
func VerifyFounding(r *Roster) error {
	if err := r.checkShape(); err != nil {
		return err
	}
	if r.Seq != 1 || len(r.PrevHash) != 0 || len(r.Members) != 1 || r.Members[0].ID != r.Signer {
		return verifyErr("roster: not a founding roster")
	}
	return r.verifySig(r)
}

// VerifyNext checks that next follows prev, a trusted roster: seq rises by
// one, prev_hash matches, the signer is an admin of prev, the epoch does
// not fall and rises when a member is dropped, and continuing members keep
// their keys.
func VerifyNext(prev, next *Roster) error {
	if err := next.checkShape(); err != nil {
		return err
	}
	if next.Team != prev.Team {
		return verifyErr("roster: team changed")
	}
	if next.Seq != prev.Seq+1 {
		return verifyErr(fmt.Sprintf("roster: seq %d does not follow %d", next.Seq, prev.Seq))
	}
	if !bytes.Equal(next.PrevHash, prev.Hash()) {
		return verifyErr("roster: prev hash does not match")
	}
	if err := next.verifySig(prev); err != nil {
		return err
	}
	if next.Epoch < prev.Epoch {
		return verifyErr("roster: epoch went down")
	}
	for _, pm := range prev.Members {
		nm, ok := next.Member(pm.ID)
		if !ok {
			if next.Epoch == prev.Epoch {
				return verifyErr("roster: member dropped without a new epoch")
			}
			continue
		}
		if !bytes.Equal(pm.SignPub, nm.SignPub) || !bytes.Equal(pm.KEMPub, nm.KEMPub) {
			return verifyErr("roster: a member's keys changed")
		}
	}
	if next.Epoch == prev.Epoch && !bytes.Equal(next.KeyCheck, prev.KeyCheck) {
		return verifyErr("roster: key check changed within an epoch")
	}
	return nil
}

// VerifyAnchor checks r as a joiner's anchor: it lists self, and it is
// signed by inviter, the admin named in the invite, with the key r gives
// for them. The id in the invite is the hash of that key, so the hub
// cannot swap it.
func VerifyAnchor(r *Roster, self, inviter string) error {
	if err := r.checkShape(); err != nil {
		return err
	}
	if _, ok := r.Member(self); !ok {
		return verifyErr("roster: does not list this member")
	}
	if r.Signer != inviter {
		return verifyErr("roster: not signed by the inviting admin")
	}
	return r.verifySig(r)
}

// Anchor finds a joiner's trust anchor in rosters (any order): the first
// roster that lists self, which VerifyAnchor must accept. Everything
// before it is history the joiner has no way to check and does not need.
func Anchor(rosters []*Roster, self, inviter string) (*Roster, error) {
	var first *Roster
	for _, r := range rosters {
		if _, ok := r.Member(self); ok && (first == nil || r.Seq < first.Seq) {
			first = r
		}
	}
	if first == nil {
		return nil, verifyErr("roster: no roster lists this member yet")
	}
	if err := VerifyAnchor(first, self, inviter); err != nil {
		return nil, err
	}
	return first, nil
}

// Advance applies the rosters after head (seq > head.Seq, any order,
// contiguous) and returns the new head. Rosters at or below head's seq are
// ignored: a reader never goes back. On an error the returned head is as
// far as the chain verified.
func Advance(head *Roster, rosters []*Roster) (*Roster, error) {
	rest := make([]*Roster, 0, len(rosters))
	for _, r := range rosters {
		if r.Seq > head.Seq {
			rest = append(rest, r)
		}
	}
	slices.SortFunc(rest, func(a, b *Roster) int { return cmp.Compare(a.Seq, b.Seq) })
	for _, r := range rest {
		if err := VerifyNext(head, r); err != nil {
			return head, err
		}
		head = r
	}
	return head, nil
}

// ---------------------------------------------------------------------------
// Building rosters

// Found makes a team's first roster, signed by k, and its first team key.
// The team id is k.Team.
func Found(k *MemberKey, teamName, memberName string) (*Roster, []byte, error) {
	K := NewTeamKey()
	m := k.Public()
	m.Admin = true
	var err error
	if m.NameSealed, err = SealName(K, k.Team, k.ID, memberName); err != nil {
		return nil, nil, err
	}
	r := &Roster{Team: k.Team, Seq: 1, Members: []Member{m}, Epoch: 1, KeyCheck: KeyCheck(k.Team, 1, K)}
	if r.TeamNameSealed, err = SealTeamName(K, k.Team, teamName); err != nil {
		return nil, nil, err
	}
	if err := r.sign(k); err != nil {
		return nil, nil, err
	}
	return r, K, nil
}

// sign sets Signer and Sig.
func (r *Roster) sign(k *MemberKey) error {
	r.Signer = k.ID
	r.Sig = ed25519.Sign(k.Sign, r.signed())
	return r.checkShape()
}

// next starts the roster after prev, to be changed and signed by signer,
// an admin of prev.
func next(prev *Roster, signer *MemberKey) (*Roster, error) {
	if prev.Team != signer.Team {
		return nil, errors.New("team: key is for another team")
	}
	if !prev.IsAdmin(signer.ID) {
		return nil, errors.New("team: only an admin can change the roster")
	}
	n := *prev
	n.Members = slices.Clone(prev.Members)
	n.Seq = prev.Seq + 1
	n.PrevHash = prev.Hash()
	n.Sig = nil
	return &n, nil
}

// AddMember makes the roster after prev with m added, named name (sealed
// with K, the team key of prev's epoch). m's admin flag is taken as is.
func AddMember(prev *Roster, signer *MemberKey, K []byte, m Member, name string) (*Roster, error) {
	n, err := next(prev, signer)
	if err != nil {
		return nil, err
	}
	if _, ok := prev.Member(m.ID); ok {
		return nil, fmt.Errorf("team: %s is a member already", m.ID)
	}
	if len(prev.Members) >= MaxMembers {
		return nil, errors.New("team: the team is full")
	}
	if m.NameSealed, err = SealName(K, prev.Team, m.ID, name); err != nil {
		return nil, err
	}
	n.Members = append(n.Members, m)
	slices.SortFunc(n.Members, func(a, b Member) int { return strings.Compare(a.ID, b.ID) })
	return n, n.sign(signer)
}

// SetAdmin makes the roster after prev with id's admin flag set to admin.
// The last admin cannot be demoted.
func SetAdmin(prev *Roster, signer *MemberKey, id string, admin bool) (*Roster, error) {
	n, err := next(prev, signer)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(n.Members, func(m Member) bool { return m.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("team: no member %s", id)
	}
	n.Members[i].Admin = admin
	if len(n.Admins()) == 0 {
		return nil, errors.New("team: a team needs an admin")
	}
	return n, n.sign(signer)
}

// RemoveMember makes the roster after prev without id, in a new epoch
// with a new team key (returned), re-sealing all names with it. K is the
// team key of prev's epoch. Admins can remove themselves if another admin
// remains.
func RemoveMember(prev *Roster, signer *MemberKey, K []byte, id string) (*Roster, []byte, error) {
	n, err := next(prev, signer)
	if err != nil {
		return nil, nil, err
	}
	i := slices.IndexFunc(n.Members, func(m Member) bool { return m.ID == id })
	if i < 0 {
		return nil, nil, fmt.Errorf("team: no member %s", id)
	}
	n.Members = slices.Delete(n.Members, i, i+1)
	if len(n.Admins()) == 0 {
		return nil, nil, errors.New("team: a team needs an admin")
	}
	newK, err := n.rotate(prev, K)
	if err != nil {
		return nil, nil, err
	}
	return n, newK, n.sign(signer)
}

// RotateEpoch makes the roster after prev with the same members in a new
// epoch with a new team key (returned), for when a key may have leaked. K
// is the team key of prev's epoch.
func RotateEpoch(prev *Roster, signer *MemberKey, K []byte) (*Roster, []byte, error) {
	n, err := next(prev, signer)
	if err != nil {
		return nil, nil, err
	}
	newK, err := n.rotate(prev, K)
	if err != nil {
		return nil, nil, err
	}
	return n, newK, n.sign(signer)
}

// rotate moves n to a new epoch with a new key, re-sealing every name
// (opened with oldK) under it.
func (n *Roster) rotate(prev *Roster, oldK []byte) ([]byte, error) {
	newK := NewTeamKey()
	n.Epoch = prev.Epoch + 1
	n.KeyCheck = KeyCheck(n.Team, n.Epoch, newK)
	for i, m := range n.Members {
		name, err := OpenName(oldK, n.Team, m.ID, m.NameSealed)
		if err != nil {
			return nil, fmt.Errorf("team: reseal name of %s: %w", m.ID, err)
		}
		if n.Members[i].NameSealed, err = SealName(newK, n.Team, m.ID, name); err != nil {
			return nil, err
		}
	}
	teamName, err := OpenTeamName(oldK, n.Team, prev.TeamNameSealed)
	if err != nil {
		return nil, fmt.Errorf("team: reseal team name: %w", err)
	}
	if n.TeamNameSealed, err = SealTeamName(newK, n.Team, teamName); err != nil {
		return nil, err
	}
	return newK, nil
}
