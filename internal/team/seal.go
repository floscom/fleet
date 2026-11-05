package team

import (
	"crypto/hmac"
	"crypto/hpke"
	"crypto/sha256"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the size of the team key and of a content key.
const KeySize = 32

// SealedKey is the team key of one epoch, sealed to one member with HPKE.
// The hub stores them and hands each member its own.
type SealedKey struct {
	Team   string `json:"team"`
	Epoch  uint64 `json:"epoch"`
	Member string `json:"member"`
	Sealed []byte `json:"sealed"`
}

// NewTeamKey returns a fresh team (or content) key.
func NewTeamKey() []byte { return randomBytes(KeySize) }

// KeyCheck is the roster's commitment to the team key of epoch:
// HMAC-SHA256(K, label | team | epoch). It is a PRF of K, so it reveals
// nothing about K, and a member opening a sealed key checks it against the
// signed roster: a hub that seals a key of its own to a member (to read
// what that member publishes next) cannot make it match.
func KeyCheck(team string, epoch uint64, K []byte) []byte {
	c := newCanon(labelKeyCheck)
	c.str(team)
	c.u64(epoch)
	m := hmac.New(sha256.New, K)
	m.Write(c.buf)
	return m.Sum(nil)
}

// teamKeyInfo is the HPKE info of a sealed team key: it binds the
// ciphertext to the team, epoch and recipient, so a sealed key moved
// between those does not open.
func teamKeyInfo(team string, epoch uint64, member string) []byte {
	c := newCanon(labelTeamKey)
	c.str(team)
	c.u64(epoch)
	c.str(member)
	return c.buf
}

// SealTeamKey seals K, the team key of epoch, to member to.
func SealTeamKey(team string, epoch uint64, K []byte, to Member) (SealedKey, error) {
	if len(K) != KeySize {
		return SealedKey{}, errors.New("team: bad team key size")
	}
	pk, err := kemPublic(to.KEMPub)
	if err != nil {
		return SealedKey{}, err
	}
	ct, err := hpke.Seal(pk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), teamKeyInfo(team, epoch, to.ID), K)
	if err != nil {
		return SealedKey{}, err
	}
	return SealedKey{Team: team, Epoch: epoch, Member: to.ID, Sealed: ct}, nil
}

// OpenTeamKey opens a team key sealed to k and checks it against roster,
// which must be a trusted roster of the same epoch (the one that
// introduced it, or any later one of that epoch). Keys of other epochs
// cannot be checked and are refused: bundles of an old epoch are re-wrapped
// by their owners, so a member only ever needs the current key.
func (k *MemberKey) OpenTeamKey(s SealedKey, roster *Roster) ([]byte, error) {
	if s.Team != k.Team || s.Member != k.ID {
		return nil, verifyErr("sealed key is not for this member")
	}
	if roster == nil || roster.Team != s.Team || roster.Epoch != s.Epoch {
		return nil, verifyErr("sealed key epoch does not match the roster")
	}
	K, err := hpke.Open(k.KEM, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), teamKeyInfo(s.Team, s.Epoch, s.Member), s.Sealed)
	if err != nil {
		return nil, verifyErr("sealed key does not open")
	}
	if len(K) != KeySize || !hmac.Equal(KeyCheck(s.Team, s.Epoch, K), roster.KeyCheck) {
		return nil, verifyErr("team key does not match the roster")
	}
	return K, nil
}

// SealKeys seals K, the team key of r's epoch, to every member of r.
func SealKeys(r *Roster, K []byte) ([]SealedKey, error) {
	out := make([]SealedKey, 0, len(r.Members))
	for _, m := range r.Members {
		s, err := SealTeamKey(r.Team, r.Epoch, K, m)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// seal encrypts plaintext under key with XChaCha20-Poly1305 and a random
// nonce, bound to aad; the box is nonce || ciphertext. XChaCha rather than
// AES-GCM because its 192-bit nonce makes random nonces safe without any
// counter: the team key wraps content keys on many machines that share no
// state, and a repeated 96-bit GCM nonce would be fatal.
func seal(key, aad, plaintext []byte) ([]byte, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := randomBytes(a.NonceSize())
	return a.Seal(nonce, nonce, plaintext, aad), nil
}

// open decrypts a box made by seal.
func open(key, aad, box []byte) ([]byte, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(box) < a.NonceSize()+a.Overhead() {
		return nil, verifyErr("sealed data too short")
	}
	pt, err := a.Open(nil, box[:a.NonceSize()], box[a.NonceSize():], aad)
	if err != nil {
		return nil, verifyErr("sealed data does not open")
	}
	return pt, nil
}

// nameAAD binds a sealed member name to its team and member.
func nameAAD(team, member string) []byte {
	c := newCanon(labelName)
	c.str(team)
	c.str(member)
	return c.buf
}

// SealName seals a member's display name with the team key, for the
// roster.
func SealName(K []byte, team, member, name string) ([]byte, error) {
	return seal(K, nameAAD(team, member), []byte(name))
}

// OpenName opens a member's display name from the roster.
func OpenName(K []byte, team, member string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	b, err := open(K, nameAAD(team, member), sealed)
	return string(b), err
}

func teamNameAAD(team string) []byte {
	c := newCanon(labelTeamName)
	c.str(team)
	return c.buf
}

// SealTeamName seals the team's display name with the team key.
func SealTeamName(K []byte, team, name string) ([]byte, error) {
	return seal(K, teamNameAAD(team), []byte(name))
}

// OpenTeamName opens the team's display name.
func OpenTeamName(K []byte, team string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	b, err := open(K, teamNameAAD(team), sealed)
	return string(b), err
}
