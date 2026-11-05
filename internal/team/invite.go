package team

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InviteScheme starts an invite string.
const InviteScheme = "fleet-team:"

// DefaultInviteTTL is how long an invite is good for unless told otherwise.
const DefaultInviteTTL = 7 * 24 * time.Hour

// Invite is what an admin hands a person to join a team: the hub, the team
// and a secret I that the hub never sees. Three keys derive from I with
// HKDF-SHA256, each with its own label and the team id as info:
//
//   - the credential, with which the joiner's one join request authenticates
//     to the hub, which stores only SHA-256(credential) (ID);
//   - the proof key, with which the joiner MACs its public keys (Proof), so
//     the hub cannot put keys of its own into the join request;
//   - the name key, which seals the joiner's display name on the way to the
//     admin (the joiner has no team key yet).
type Invite struct {
	Hub    string // hub base URL
	Team   string // team id
	Secret []byte // I, 32 bytes
	Admin  string // member id of the inviting admin: the joiner's trust anchor signer
}

// NewInvite makes an invite to team on hub from admin.
func NewInvite(hub, team, admin string) (*Invite, error) {
	if !ValidID(team) || !ValidID(admin) {
		return nil, errors.New("team: bad team or admin id")
	}
	hub = strings.TrimRight(hub, "/")
	if !strings.HasPrefix(hub, "https://") && !strings.HasPrefix(hub, "http://") {
		return nil, errors.New("team: hub must be an http(s) URL")
	}
	return &Invite{Hub: hub, Team: team, Secret: randomBytes(KeySize), Admin: admin}, nil
}

// String formats the invite:
// fleet-team:<hub>/<team>#<base64url(I)>.<admin id>
func (i *Invite) String() string {
	return InviteScheme + i.Hub + "/" + i.Team + "#" + base64.RawURLEncoding.EncodeToString(i.Secret) + "." + i.Admin
}

// ParseInvite parses what String made (surrounding space allowed).
func ParseInvite(s string) (*Invite, error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, InviteScheme)
	if !ok {
		return nil, errors.New("team: not a fleet-team invite")
	}
	loc, frag, ok := strings.Cut(rest, "#")
	if !ok {
		return nil, errors.New("team: invite has no secret part")
	}
	cut := strings.LastIndex(loc, "/")
	if cut < 0 {
		return nil, errors.New("team: invite has no team id")
	}
	hub, team := loc[:cut], loc[cut+1:]
	secretB64, admin, ok := strings.Cut(frag, ".")
	if !ok {
		return nil, errors.New("team: invite names no admin")
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretB64)
	if err != nil || len(secret) != KeySize {
		return nil, errors.New("team: invite secret is malformed")
	}
	if !ValidID(team) || !ValidID(admin) {
		return nil, errors.New("team: invite has a bad team or admin id")
	}
	if !strings.HasPrefix(hub, "https://") && !strings.HasPrefix(hub, "http://") {
		return nil, errors.New("team: invite hub is not an http(s) URL")
	}
	return &Invite{Hub: hub, Team: team, Secret: secret, Admin: admin}, nil
}

// derive is HKDF-SHA256(I, no salt, label | team) of KeySize bytes.
func (i *Invite) derive(label string) []byte {
	c := newCanon(label)
	c.str(i.Team)
	k, err := hkdf.Key(sha256.New, i.Secret, nil, string(c.buf), KeySize)
	if err != nil {
		panic("team: hkdf: " + err.Error())
	}
	return k
}

// Credential is what the joiner sends in "Authorization: Fleet-Invite
// <credential>": base64url of the derived credential key.
func (i *Invite) Credential() string {
	return base64.RawURLEncoding.EncodeToString(i.derive(labelInviteAuth))
}

// ID identifies the invite on the hub: hex SHA-256 of the raw credential.
// The hub learns it when the admin registers the invite and checks join
// requests against it; it cannot get the credential back from it.
func (i *Invite) ID() string {
	return InviteID(i.derive(labelInviteAuth))
}

// InviteID is the invite id for a raw credential (what the hub computes
// from the Fleet-Invite header).
func InviteID(credential []byte) string {
	sum := sha256.Sum256(credential)
	return hex.EncodeToString(sum[:])
}

// ParseCredential decodes a Fleet-Invite header value.
func ParseCredential(s string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return b, err == nil && len(b) == KeySize
}

// proofMessage is what the joiner MACs: team | sign_pub | kem_pub.
func (i *Invite) proofMessage(m Member) []byte {
	c := newCanon(labelInvitePrf)
	c.str(i.Team)
	c.bytes(m.SignPub)
	c.bytes(m.KEMPub)
	return c.buf
}

func (i *Invite) proof(m Member) []byte {
	mac := hmac.New(sha256.New, i.derive(labelInvitePrf))
	mac.Write(i.proofMessage(m))
	return mac.Sum(nil)
}

// NewJoinRequest makes k's join request for this invite, with k's display
// name sealed for the admin.
func (i *Invite) NewJoinRequest(k *MemberKey, name string) (JoinRequest, error) {
	if k.Team != i.Team {
		return JoinRequest{}, errors.New("team: key is for another team")
	}
	m := k.Public()
	sealedName, err := seal(i.derive(labelInviteName), i.proofMessage(m), []byte(name))
	if err != nil {
		return JoinRequest{}, err
	}
	return JoinRequest{SignPub: m.SignPub, KEMPub: m.KEMPub, Proof: i.proof(m), NameSealed: sealedName}, nil
}

// VerifyJoin checks a join request the hub relayed against this invite and
// returns the joiner as a Member (not an admin, name not yet sealed for
// the roster) and their display name. The admin then adds them with
// AddMember.
func (i *Invite) VerifyJoin(j JoinRequest) (Member, string, error) {
	m := Member{SignPub: j.SignPub, KEMPub: j.KEMPub}
	if len(m.SignPub) != 32 {
		return Member{}, "", verifyErr("join: bad signing key")
	}
	m.ID = MemberID(m.SignPub)
	if _, err := kemPublic(m.KEMPub); err != nil {
		return Member{}, "", fmt.Errorf("join: %w", err)
	}
	if !hmac.Equal(j.Proof, i.proof(m)) {
		return Member{}, "", verifyErr("join: proof does not match the invite")
	}
	name, err := open(i.derive(labelInviteName), i.proofMessage(m), j.NameSealed)
	if err != nil {
		return Member{}, "", fmt.Errorf("join: name: %w", err)
	}
	return m, string(name), nil
}
