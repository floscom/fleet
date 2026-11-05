// Package team is the cryptography and the wire protocol of fleet teams:
// member keys, the sealed team key, signed rosters, sealed skill bundles,
// invites, and the hub's API types and client. It has no daemon logic and
// no storage of its own; the gateway (daemon side) and the hub
// (internal/hub) are both built on it.
//
// # Keys
//
// A MemberKey is one fleet's identity in one team: an Ed25519 signing key
// and an HPKE key pair (KEM MLKEM768-X25519, KDF HKDF-SHA256, AEAD
// ChaCha20-Poly1305). Its member id is the first 16 bytes of
// SHA-256(signing public key), as hex. Members of a team share a team key
// K[e], 32 random bytes per epoch e; SealTeamKey seals it to one member
// with HPKE, and MemberKey.OpenTeamKey opens it, checking it against the
// roster's KeyCheck so a hub cannot hand a member a key of its own making.
// A new epoch starts whenever a member goes (RemoveMember, RotateEpoch).
//
// # Rosters
//
// A Roster is the member list at one seq, signed by an admin over a
// canonical binary encoding (not JSON). Found makes seq 1; AddMember,
// RemoveMember, SetAdmin and RotateEpoch make the next one; SealKeys seals
// the team key for everyone in it. Readers never trust a roster the hub
// hands them as such; they trust an anchor and verify the chain from
// there:
//
//   - the founder's anchor is its own seq 1 (VerifyFounding);
//   - a joiner's anchor is the first roster that lists the joiner, which
//     must be signed by the admin named in the invite (Anchor). History
//     before it is not trusted, so a hub cannot fabricate a chain from seq
//     1 with its own founder key and add the real admin later;
//   - from the anchor on, Advance checks each roster: seq rises by one,
//     prev_hash matches, the signer is an admin of the previous roster, the
//     epoch never falls and rises when a member is dropped, continuing
//     members keep their keys. A reader keeps the highest seq it has seen
//     and takes nothing lower.
//
// Display names are sealed with the team key (SealName), the team's name
// too, so the hub sees ids but no names.
//
// # Bundles
//
// A Head is one version of a published skill: who owns it, a content key C
// wrapped with K[epoch] (KeyWrap), the metadata sealed with C (MetaSealed:
// kind, name, description, hash, size, files) and the content address of
// its data (DataRef, the tar.gz sealed with C and stored on the hub as an
// opaque blob), signed by the owner. SealBundle makes a new version,
// Rewrap one for a new epoch without touching the data, Tombstone a
// deletion. VerifyHead checks a head against a trusted roster, VerifyData
// the downloaded blob against the head, OpenBundle opens both. Versions
// are Unix milliseconds and only ever rise per bundle (NextVersion).
//
// # Invites
//
// An Invite carries a 32-byte secret I; from it come three keys (HKDF):
// the hub credential (the hub stores only its hash, single use, with an
// expiry), the proof key (the joiner MACs its public keys with it so the
// hub cannot substitute them) and the name key (the joiner's display name
// travels sealed with it, since the joiner has no team key yet). The
// inviting admin verifies a JoinRequest with Invite.VerifyJoin and adds
// the member to the roster.
//
// # Hub API
//
// api.go has the request and reply types the hub and its clients share
// and the request signature (SignRequest, VerifyRequest): a member request
// carries "Authorization: Fleet-Member <id>.<nonce>.<hex sig>", the nonce
// from GET /v1/nonce. client.go has Client, the hub client the gateway
// uses. The endpoints are listed in docs/TEAMS.md.
package team

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// Domain labels of everything signed, hashed or derived. A label is the
// first field of a canonical encoding, so no two constructions share a
// byte string. Changing one changes the protocol: docs/TEAMS.md lists
// them, and vectors_test.go pins them.
const (
	labelRoster     = "fleet-team-roster-v1"
	labelHead       = "fleet-team-head-v1"
	labelTeamKey    = "fleet-team-key-v1"
	labelKeyCheck   = "fleet-team-key-check-v1"
	labelWrap       = "fleet-team-wrap-v1"
	labelMeta       = "fleet-team-meta-v1"
	labelData       = "fleet-team-data-v1"
	labelName       = "fleet-team-name-v1"
	labelTeamName   = "fleet-team-team-name-v1"
	labelInviteAuth = "fleet-team-invite-auth-v1"
	labelInvitePrf  = "fleet-team-invite-proof-v1"
	labelInviteName = "fleet-team-invite-name-v1"
	labelRequest    = "fleet-team-member-v1"
)

// ErrVerify is wrapped by every verification failure, so callers can tell
// "this is bad" from "I could not check".
var ErrVerify = errors.New("team: verification failed")

// verifyErr returns a verification error with a reason.
func verifyErr(reason string) error {
	return &verifyError{reason}
}

type verifyError struct{ reason string }

func (e *verifyError) Error() string     { return "team: " + e.reason }
func (e *verifyError) Is(err error) bool { return err == ErrVerify }

// canon builds a canonical, length-prefixed binary encoding: every string
// and byte field goes as a big-endian uint32 length and the bytes, numbers
// as big-endian uint64, booleans as one byte, lists as a count and the
// items. The first field is always a domain label, so encodings of
// different things never collide, whatever their fields.
type canon struct{ buf []byte }

func newCanon(label string) *canon {
	c := &canon{}
	c.str(label)
	return c
}

func (c *canon) bytes(b []byte) {
	c.u32(uint32(len(b)))
	c.buf = append(c.buf, b...)
}

func (c *canon) str(s string) { c.bytes([]byte(s)) }

func (c *canon) u32(v uint32) { c.buf = binary.BigEndian.AppendUint32(c.buf, v) }

func (c *canon) u64(v uint64) { c.buf = binary.BigEndian.AppendUint64(c.buf, v) }

func (c *canon) boolean(b bool) {
	if b {
		c.buf = append(c.buf, 1)
	} else {
		c.buf = append(c.buf, 0)
	}
}

// NewID returns a random 128-bit id as 32 lowercase hex digits: team ids,
// bundle ids and data refs look alike.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("team: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ValidID reports whether s is an id as NewID makes them (and as member
// ids are): 32 lowercase hex digits.
func ValidID(s string) bool {
	return len(s) == 32 && isLowerHex(s)
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// randomBytes returns n random bytes or panics: without randomness nothing
// here is safe to continue with.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("team: crypto/rand failed: " + err.Error())
	}
	return b
}

// writeFileAtomic writes data to a 0600 temp file next to path, syncs it
// and renames it into place, creating the parent directory (0700) if
// needed.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
