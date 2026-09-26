package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// crockford is the Crockford base32 alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeLen is the number of characters in a pairing code (8 x 5 = 40 bits).
const codeLen = 8

// maxFailedAttempts is how many failed redemptions invalidate a code.
const maxFailedAttempts = 5

// NormalizeCode upper-cases, strips dashes/spaces and maps Crockford
// look-alikes (O->0, I/L->1). Returns "" if the result is not 8 valid chars.
func NormalizeCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(crockford, r) {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != codeLen {
		return ""
	}
	return b.String()
}

// PairProof computes the client pairing proof.
// HMAC-SHA256(key=normalized code, "fleet-pair-v1" || serverFP || devicePub).
func PairProof(code string, serverFP, devicePub []byte) []byte {
	return pairMAC("fleet-pair-v1", code, serverFP, devicePub)
}

// ServerPairProof computes the server's proof.
// HMAC-SHA256(key=normalized code, "fleet-pair-server-v1" || serverFP || devicePub).
func ServerPairProof(code string, serverFP, devicePub []byte) []byte {
	return pairMAC("fleet-pair-server-v1", code, serverFP, devicePub)
}

func pairMAC(label, code string, serverFP, devicePub []byte) []byte {
	m := hmac.New(sha256.New, []byte(NormalizeCode(code)))
	m.Write([]byte(label))
	m.Write(serverFP)
	m.Write(devicePub)
	return m.Sum(nil)
}

// PairingCodes issues and checks single-use codes. Safe for concurrent use.
type PairingCodes struct {
	mu    sync.Mutex
	codes map[string]*pendingCode
	now   func() time.Time
}

type pendingCode struct {
	expires time.Time
	failed  int
}

// NewPairingCodes returns an empty code book.
func NewPairingCodes() *PairingCodes {
	return &PairingCodes{codes: map[string]*pendingCode{}, now: time.Now}
}

// Issue creates a new code valid for ttl and returns its display form and
// expiry. Codes issued before are invalidated: a proof captured by an
// impostor could be brute-forced offline, so at most one code is ever live.
func (p *PairingCodes) Issue(ttl time.Duration) (display string, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.codes)
	code := randomCode()
	expires = p.now().Add(ttl)
	p.codes[code] = &pendingCode{expires: expires}
	return code[:4] + "-" + code[4:], expires
}

// Redeem finds a live code whose PairProof matches proof (constant-time
// compare), consumes it and returns the normalized code. Each failed call
// counts one attempt against every live code; codes are invalidated after 5
// failed attempts.
func (p *PairingCodes) Redeem(proof, serverFP, devicePub []byte) (code string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked()
	for c := range p.codes {
		if hmac.Equal(proof, PairProof(c, serverFP, devicePub)) {
			code, ok = c, true
		}
	}
	if ok {
		delete(p.codes, code)
		return code, true
	}
	for c, pc := range p.codes {
		if pc.failed++; pc.failed >= maxFailedAttempts {
			delete(p.codes, c)
		}
	}
	return "", false
}

// pruneLocked drops expired codes.
func (p *PairingCodes) pruneLocked() {
	now := p.now()
	for c, pc := range p.codes {
		if !now.Before(pc.expires) {
			delete(p.codes, c)
		}
	}
}

// randomCode returns 8 random Crockford base32 characters (40 bits).
func randomCode() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("identity: crypto/rand failed: " + err.Error())
	}
	v := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	out := make([]byte, codeLen)
	for i := codeLen - 1; i >= 0; i-- {
		out[i] = crockford[v&31]
		v >>= 5
	}
	return string(out)
}
