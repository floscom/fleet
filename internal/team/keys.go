package team

import (
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// kemPublicSize is the serialized MLKEM768-X25519 public key: 1184 bytes of
// ML-KEM-768 and 32 of X25519.
const kemPublicSize = 1216

// kem is the HPKE KEM of every member key: a hybrid of ML-KEM-768 and
// X25519 (X-Wing), so a wrapped team key stays secret even against a
// quantum computer as long as either half holds.
func kem() hpke.KEM { return hpke.MLKEM768X25519() }

// MemberKey is one fleet's identity in one team. Two teams never share a
// key, so two hubs cannot link one fleet's memberships.
type MemberKey struct {
	// Hub is the base URL of the team's hub, Team the team id.
	Hub  string
	Team string
	// ID is the member id: the first 16 bytes of SHA-256(Sign's public
	// key), hex.
	ID   string
	Sign ed25519.PrivateKey
	KEM  hpke.PrivateKey
}

// NewMemberKey makes a fresh key for team on hub.
func NewMemberKey(hub, team string) (*MemberKey, error) {
	if !ValidID(team) {
		return nil, fmt.Errorf("team: bad team id %q", team)
	}
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	k, err := kem().GenerateKey()
	if err != nil {
		return nil, err
	}
	return &MemberKey{Hub: hub, Team: team, ID: MemberID(sign.Public().(ed25519.PublicKey)), Sign: sign, KEM: k}, nil
}

// MemberID derives a member id from a signing public key.
func MemberID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// Public is the member as it goes into a roster: id and public keys, not
// an admin, no name yet.
func (k *MemberKey) Public() Member {
	return Member{
		ID:      k.ID,
		SignPub: []byte(k.Sign.Public().(ed25519.PublicKey)),
		KEMPub:  k.KEM.PublicKey().Bytes(),
	}
}

// MemberFromKeys makes the Member for a pair of public keys, checking them:
// what the hub does with a join request's keys.
func MemberFromKeys(signPub, kemPub []byte) (Member, error) {
	if len(signPub) != ed25519.PublicKeySize {
		return Member{}, verifyErr("bad signing public key size")
	}
	if _, err := kemPublic(kemPub); err != nil {
		return Member{}, err
	}
	return Member{ID: MemberID(signPub), SignPub: signPub, KEMPub: kemPub}, nil
}

// memberKeyFile is the on-disk form: both keys as their 32-byte seeds.
type memberKeyFile struct {
	Version  int    `json:"v"`
	Hub      string `json:"hub"`
	Team     string `json:"team"`
	ID       string `json:"id"`
	SignSeed []byte `json:"sign_seed"`
	KEMSeed  []byte `json:"kem_seed"`
}

// MarshalJSON writes the key file form.
func (k *MemberKey) MarshalJSON() ([]byte, error) {
	kemSeed, err := k.KEM.Bytes()
	if err != nil {
		return nil, err
	}
	return json.Marshal(memberKeyFile{Version: 1, Hub: k.Hub, Team: k.Team, ID: k.ID, SignSeed: k.Sign.Seed(), KEMSeed: kemSeed})
}

// UnmarshalJSON reads the key file form and checks it against itself.
func (k *MemberKey) UnmarshalJSON(data []byte) error {
	var f memberKeyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.Version != 1 {
		return fmt.Errorf("team: member key version %d not supported", f.Version)
	}
	if len(f.SignSeed) != ed25519.SeedSize {
		return errors.New("team: member key: bad signing seed")
	}
	kemKey, err := kem().NewPrivateKey(f.KEMSeed)
	if err != nil {
		return fmt.Errorf("team: member key: %w", err)
	}
	sign := ed25519.NewKeyFromSeed(f.SignSeed)
	id := MemberID(sign.Public().(ed25519.PublicKey))
	if f.ID != "" && f.ID != id {
		return errors.New("team: member key: id does not match the key")
	}
	if !ValidID(f.Team) {
		return fmt.Errorf("team: member key: bad team id %q", f.Team)
	}
	*k = MemberKey{Hub: f.Hub, Team: f.Team, ID: id, Sign: sign, KEM: kemKey}
	return nil
}

// Save writes the key to path (0600, atomically).
func (k *MemberKey) Save(path string) error {
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// LoadMemberKey reads a key written by Save.
func LoadMemberKey(path string) (*MemberKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k := &MemberKey{}
	if err := json.Unmarshal(data, k); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// kemPublic parses a member's HPKE public key.
func kemPublic(b []byte) (hpke.PublicKey, error) {
	if len(b) != kemPublicSize {
		return nil, verifyErr("bad KEM public key size")
	}
	pk, err := kem().NewPublicKey(b)
	if err != nil {
		return nil, verifyErr("bad KEM public key: " + err.Error())
	}
	return pk, nil
}
