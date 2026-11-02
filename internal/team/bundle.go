package team

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxBundleData caps a bundle's data (the sealed tar.gz), as a skill is at
// most 16 MiB; the seal adds a few dozen bytes.
const MaxBundleData = 16<<20 + 1024

// Meta is a bundle's metadata, sealed into Head.MetaSealed.
type Meta struct {
	// Kind is what the bundle holds: "skill" for now.
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Hash and Size are the skill's content hash and size as in the skill
	// store (skills.Entry), Files its file list.
	Hash  string   `json:"hash"`
	Size  int64    `json:"size"`
	Files []string `json:"files,omitempty"`
}

// Head is one version of a published bundle, signed by its owner. The hub
// keeps the latest head per bundle id and the blob DataRef points to.
type Head struct {
	Team string `json:"team"`
	// ID is the bundle's id, random and stable for the life of the skill.
	ID    string `json:"id"`
	Owner string `json:"owner"`
	// Version is the Unix millisecond time of the change; it only rises.
	Version int64 `json:"version"`
	// Epoch is the team key epoch KeyWrap is wrapped with.
	Epoch uint64 `json:"epoch"`
	// KeyWrap is the content key C sealed with K[Epoch].
	KeyWrap []byte `json:"key_wrap,omitempty"`
	// MetaSealed is the JSON Meta sealed with C.
	MetaSealed []byte `json:"meta_sealed,omitempty"`
	// DataRef is the content address of the data (the tar.gz sealed with
	// C): hex SHA-256 of that ciphertext, which is also its name on the
	// hub. DataSize is its size.
	DataRef  string `json:"data_ref,omitempty"`
	DataSize int64  `json:"data_size,omitempty"`
	// Deleted marks a tombstone: the bundle is withdrawn, and the fields
	// above but Version are empty.
	Deleted bool   `json:"deleted,omitempty"`
	Sig     []byte `json:"sig"`
}

// signed is the canonical encoding the owner signs.
func (h *Head) signed() []byte {
	c := newCanon(labelHead)
	c.str(h.Team)
	c.str(h.ID)
	c.str(h.Owner)
	c.u64(uint64(h.Version))
	c.u64(h.Epoch)
	c.bytes(h.KeyWrap)
	c.bytes(h.MetaSealed)
	c.str(h.DataRef)
	c.u64(uint64(h.DataSize))
	c.boolean(h.Deleted)
	return c.buf
}

// checkShape checks a head on its own.
func (h *Head) checkShape() error {
	if !ValidID(h.Team) || !ValidID(h.ID) || !ValidID(h.Owner) {
		return verifyErr("head: bad ids")
	}
	if h.Version <= 0 {
		return verifyErr("head: bad version")
	}
	if len(h.Sig) != ed25519.SignatureSize {
		return verifyErr("head: bad signature size")
	}
	if h.Deleted {
		if len(h.KeyWrap) != 0 || len(h.MetaSealed) != 0 || h.DataRef != "" || h.DataSize != 0 {
			return verifyErr("head: tombstone carries data")
		}
		return nil
	}
	if len(h.KeyWrap) == 0 || len(h.MetaSealed) == 0 {
		return verifyErr("head: missing key wrap or metadata")
	}
	if len(h.DataRef) != 2*sha256.Size || !isLowerHex(h.DataRef) {
		return verifyErr("head: bad data ref")
	}
	if h.DataSize <= 0 || h.DataSize > MaxBundleData {
		return verifyErr("head: bad data size")
	}
	return nil
}

// VerifyHead checks h against roster, a trusted roster: the owner is a
// member, the signature is theirs, and the head is well formed. The caller
// keeps the highest version it has seen per bundle and takes nothing lower.
func VerifyHead(h *Head, roster *Roster) error {
	if err := h.checkShape(); err != nil {
		return err
	}
	if h.Team != roster.Team {
		return verifyErr("head: wrong team")
	}
	owner, ok := roster.Member(h.Owner)
	if !ok {
		return verifyErr("head: owner is not a member")
	}
	if !ed25519.Verify(owner.SignPub, h.signed(), h.Sig) {
		return verifyErr("head: bad signature")
	}
	if !h.Deleted && h.Epoch > roster.Epoch {
		return verifyErr("head: epoch from the future")
	}
	return nil
}

// DataRef is the content address of sealed data.
func DataRef(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifyData checks downloaded data against h.
func VerifyData(h *Head, data []byte) error {
	if h.Deleted || int64(len(data)) != h.DataSize || DataRef(data) != h.DataRef {
		return verifyErr("head: data does not match")
	}
	return nil
}

// NextVersion returns a version above prev: now, or prev+1 if the clock is
// behind. Zero prev means a first version.
func NextVersion(prev int64) int64 {
	return max(time.Now().UnixMilli(), prev+1)
}

// wrapAAD binds a wrapped content key to its head.
func wrapAAD(team, id string, version int64, epoch uint64) []byte {
	c := newCanon(labelWrap)
	c.str(team)
	c.str(id)
	c.u64(uint64(version))
	c.u64(epoch)
	return c.buf
}

// contentAAD binds sealed metadata or data to its bundle (not its version:
// a re-wrap keeps both).
func contentAAD(label, team, id string) []byte {
	c := newCanon(label)
	c.str(team)
	c.str(id)
	return c.buf
}

// SealBundle makes a new version of bundle id, owned by owner, with a fresh
// content key wrapped under K, the team key of epoch. It returns the signed
// head and the sealed data to upload under head.DataRef. version must be
// above the bundle's previous version (NextVersion).
func SealBundle(owner *MemberKey, id string, version int64, epoch uint64, K []byte, meta Meta, data []byte) (*Head, []byte, error) {
	if !ValidID(id) {
		return nil, nil, errors.New("team: bad bundle id")
	}
	if len(data) > MaxBundleData-1024 {
		return nil, nil, errors.New("team: bundle data too large")
	}
	C := NewTeamKey()
	h := &Head{Team: owner.Team, ID: id, Owner: owner.ID, Version: version, Epoch: epoch}
	var err error
	if h.KeyWrap, err = seal(K, wrapAAD(h.Team, id, version, epoch), C); err != nil {
		return nil, nil, err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, nil, err
	}
	if h.MetaSealed, err = seal(C, contentAAD(labelMeta, h.Team, id), metaJSON); err != nil {
		return nil, nil, err
	}
	sealedData, err := seal(C, contentAAD(labelData, h.Team, id), data)
	if err != nil {
		return nil, nil, err
	}
	h.DataRef, h.DataSize = DataRef(sealedData), int64(len(sealedData))
	return h, sealedData, h.sign(owner)
}

func (h *Head) sign(owner *MemberKey) error {
	h.Sig = ed25519.Sign(owner.Sign, h.signed())
	return h.checkShape()
}

// Rewrap makes the version after prev with the same metadata and data but
// the content key wrapped under newK, the team key of epoch, so members of
// the new epoch can open it. oldK is the key of prev.Epoch. Nothing is
// uploaded again.
func Rewrap(prev *Head, owner *MemberKey, oldK []byte, version int64, epoch uint64, newK []byte) (*Head, error) {
	if prev.Owner != owner.ID || prev.Deleted {
		return nil, errors.New("team: not an own, live bundle")
	}
	C, err := open(oldK, wrapAAD(prev.Team, prev.ID, prev.Version, prev.Epoch), prev.KeyWrap)
	if err != nil {
		return nil, err
	}
	h := *prev
	h.Version, h.Epoch = version, epoch
	if h.KeyWrap, err = seal(newK, wrapAAD(h.Team, h.ID, version, epoch), C); err != nil {
		return nil, err
	}
	return &h, h.sign(owner)
}

// Tombstone makes the version after prev that withdraws the bundle.
func Tombstone(prev *Head, owner *MemberKey, version int64) (*Head, error) {
	if prev.Owner != owner.ID {
		return nil, errors.New("team: not an own bundle")
	}
	h := &Head{Team: prev.Team, ID: prev.ID, Owner: prev.Owner, Version: version, Deleted: true}
	return h, h.sign(owner)
}

// OpenMeta opens h's metadata with K, the team key of h.Epoch.
func OpenMeta(h *Head, K []byte) (Meta, error) {
	C, err := h.contentKey(K)
	if err != nil {
		return Meta{}, err
	}
	b, err := open(C, contentAAD(labelMeta, h.Team, h.ID), h.MetaSealed)
	if err != nil {
		return Meta{}, fmt.Errorf("metadata: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, verifyErr("metadata: bad JSON")
	}
	return m, nil
}

// OpenBundle opens h's metadata and data (as downloaded, checked with
// VerifyData first) with K, the team key of h.Epoch.
func OpenBundle(h *Head, K []byte, data []byte) (Meta, []byte, error) {
	if err := VerifyData(h, data); err != nil {
		return Meta{}, nil, err
	}
	m, err := OpenMeta(h, K)
	if err != nil {
		return Meta{}, nil, err
	}
	C, err := h.contentKey(K)
	if err != nil {
		return Meta{}, nil, err
	}
	pt, err := open(C, contentAAD(labelData, h.Team, h.ID), data)
	if err != nil {
		return Meta{}, nil, fmt.Errorf("data: %w", err)
	}
	return m, pt, nil
}

// contentKey unwraps C with the team key of h.Epoch.
func (h *Head) contentKey(K []byte) ([]byte, error) {
	if h.Deleted {
		return nil, errors.New("team: bundle is deleted")
	}
	C, err := open(K, wrapAAD(h.Team, h.ID, h.Version, h.Epoch), h.KeyWrap)
	if err != nil {
		return nil, fmt.Errorf("key wrap: %w", err)
	}
	return C, nil
}
