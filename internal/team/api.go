package team

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Request authentication.
//
// A member signs each request to the hub, the way daemons sign requests to
// each other with the fleet key (web/peers.go), but with its Ed25519 key:
//
//	Authorization: Fleet-Member <member id>.<nonce>.<hex signature>
//
// The nonce comes from GET /v1/nonce (single use, short-lived). The
// signature is over labelRequest "\n" team id "\n" nonce "\n" method "\n"
// request URI "\n" hex(SHA-256(body)). The team id is in the URI already;
// it is repeated so a signature is good for one team only even if a hub's
// routing changed. The hub looks the member up in the team's current
// roster, so a removed member's signatures stop working with the roster
// that removed them.
//
// Join requests authenticate with the invite credential instead:
//
//	Authorization: Fleet-Invite <base64url credential>
//
// and team creation with a one-time code from the hub operator:
//
//	Authorization: Fleet-Create <code>
const (
	MemberScheme     = "Fleet-Member "
	InviteAuthScheme = "Fleet-Invite "
	CreateScheme     = "Fleet-Create "
)

// RequestMessage is the byte string a member signs for one request.
func RequestMessage(team, nonce, method, uri string, bodySum []byte) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", labelRequest, team, nonce, method, uri, hex.EncodeToString(bodySum)))
}

// SignRequest returns the Authorization header value for one request.
func (k *MemberKey) SignRequest(nonce, method, uri string, body []byte) string {
	sum := sha256.Sum256(body)
	sig := ed25519.Sign(k.Sign, RequestMessage(k.Team, nonce, method, uri, sum[:]))
	return MemberScheme + k.ID + "." + nonce + "." + hex.EncodeToString(sig)
}

// ParseMemberAuth splits a Fleet-Member header value.
func ParseMemberAuth(header string) (id, nonce string, sig []byte, ok bool) {
	v, ok := strings.CutPrefix(header, MemberScheme)
	if !ok {
		return "", "", nil, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 || !ValidID(parts[0]) || parts[1] == "" {
		return "", "", nil, false
	}
	sig, err := hex.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", "", nil, false
	}
	return parts[0], parts[1], sig, true
}

// VerifyRequest checks a request signature against a member's key.
func VerifyRequest(pub ed25519.PublicKey, team, nonce, method, uri string, bodySum, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, RequestMessage(team, nonce, method, uri, bodySum), sig)
}

// ---------------------------------------------------------------------------
// Request and reply bodies

// Error is the hub's error reply.
type Error struct {
	Error string `json:"error"`
}

// NonceReply is GET /v1/nonce.
type NonceReply struct {
	Nonce string `json:"nonce"`
}

// CreateRequest is POST /v1/teams: the founding roster and the founder's
// own sealed key.
type CreateRequest struct {
	Roster *Roster     `json:"roster"`
	Keys   []SealedKey `json:"keys"`
}

// RosterPost is POST /v1/teams/{t}/roster: the next roster and, when it
// starts an epoch or adds members, their sealed keys, stored together so
// no member sees the roster without its key.
type RosterPost struct {
	Roster *Roster     `json:"roster"`
	Keys   []SealedKey `json:"keys,omitempty"`
}

// RosterList is GET /v1/teams/{t}/roster?since=N: the rosters above seq N
// in order.
type RosterList struct {
	Rosters []*Roster `json:"rosters"`
}

// KeyList is GET and POST /v1/teams/{t}/keys.
type KeyList struct {
	Keys []SealedKey `json:"keys"`
}

// InviteRegistration is POST /v1/teams/{t}/invites: the invite's id (hash
// of its credential) and when it expires.
type InviteRegistration struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
}

// JoinRequest is POST /v1/teams/{t}/joins, and an item of JoinList. The
// hub fills in ID (the member id the keys give), Invite and Created.
type JoinRequest struct {
	ID         string    `json:"id,omitempty"`
	Invite     string    `json:"invite,omitempty"`
	SignPub    []byte    `json:"sign_pub"`
	KEMPub     []byte    `json:"kem_pub"`
	Proof      []byte    `json:"proof"`
	NameSealed []byte    `json:"name_sealed"`
	Created    time.Time `json:"created,omitempty"`
}

// JoinList is GET /v1/teams/{t}/joins (admins): pending join requests and
// the members who left and are still on the roster, for the admin to
// remove.
type JoinList struct {
	Joins []JoinRequest `json:"joins"`
	Left  []string      `json:"left"`
}

// HeadList is GET /v1/teams/{t}/bundles.
type HeadList struct {
	Heads []*Head `json:"heads"`
}

// Event kinds of GET /v1/teams/{t}/events.
const (
	EventRoster = "roster" // Ref: the new seq
	EventKeys   = "keys"   // Member: who got a sealed key (seen by them and admins)
	EventJoin   = "join"   // Ref: the join request's id (admins)
	EventLeft   = "left"   // Ref: the member who left (admins)
	EventBundle = "bundle" // Ref: the bundle id
)

// Event is one entry of a team's event log.
type Event struct {
	Seq    uint64    `json:"seq"`
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Ref    string    `json:"ref,omitempty"`
	Member string    `json:"member,omitempty"`
}

// EventList is GET /v1/teams/{t}/events?since=N. Next is the cursor for
// the next call. Reset means the hub no longer has events back to N; the
// client should fetch rosters, keys and heads afresh.
type EventList struct {
	Events []Event `json:"events"`
	Next   uint64  `json:"next"`
	Reset  bool    `json:"reset,omitempty"`
}

// CodeReply is the hub operator's POST /v1/admin/codes.
type CodeReply struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}
