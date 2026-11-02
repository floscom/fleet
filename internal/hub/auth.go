package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"fleet/internal/team"
)

// nonces are the outstanding single-use nonces, from GET /v1/nonce and
// from NonceHeader.
type nonces struct {
	mu    sync.Mutex
	m     map[string]nonceRec // nonce -> expiry and issuing address
	perIP map[string]int      // outstanding nonces per address
	now   func() time.Time
}

type nonceRec struct {
	exp time.Time
	ip  string
}

// issue returns a new nonce for ip, or false when too many are
// outstanding, in all or for ip.
func (n *nonces) issue(ip string) (string, bool) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.m == nil {
		n.m, n.perIP = map[string]nonceRec{}, map[string]int{}
	}
	if len(n.m) >= maxNonces || n.perIP[ip] >= maxNoncesPerIP {
		n.pruneLocked(now)
		if len(n.m) >= maxNonces || n.perIP[ip] >= maxNoncesPerIP {
			return "", false
		}
	}
	nonce := hex.EncodeToString(b[:])
	n.m[nonce] = nonceRec{exp: now.Add(nonceTTL), ip: ip}
	n.perIP[ip]++
	return nonce, true
}

// take consumes nonce and reports whether it was issued and is unexpired.
func (n *nonces) take(nonce string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	rec, ok := n.m[nonce]
	if ok {
		n.forgetLocked(nonce, rec)
	}
	return ok && n.now().Before(rec.exp)
}

// pruneLocked drops expired nonces.
func (n *nonces) pruneLocked(now time.Time) {
	for k, rec := range n.m {
		if now.After(rec.exp) {
			n.forgetLocked(k, rec)
		}
	}
}

func (n *nonces) forgetLocked(nonce string, rec nonceRec) {
	delete(n.m, nonce)
	if n.perIP[rec.ip]--; n.perIP[rec.ip] <= 0 {
		delete(n.perIP, rec.ip)
	}
}

func (s *Server) handleNonce(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.nonceLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	nonce, ok := s.nonces.issue(ip)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many requests")
		return
	}
	writeJSON(w, http.StatusOK, team.NonceReply{Nonce: nonce})
}

// chainNonce puts a nonce for the client's next request into the response
// headers (must be called before the body is written). A member that has
// just proven itself with a signature is not rate limited here; the
// per-address cap still bounds what it can hold.
func (s *Server) chainNonce(w http.ResponseWriter, r *http.Request) {
	if nonce, ok := s.nonces.issue(s.clientIP(r)); ok {
		w.Header().Set(NonceHeader, nonce)
	}
}

// authed is what a signed request established.
type authed struct {
	team   *teamState
	member team.Member
	body   []byte
}

// handler is a handler of an authenticated request. The server lock is
// held while it runs, so handlers that wait (events) must release it.
type handler func(w http.ResponseWriter, r *http.Request, a *authed)

// member wraps h with member authentication: the request is signed by a
// member of the team's current roster who has not been revoked. The body
// is read (capped) and handed over. The nonce is spent either way: one
// guess per nonce.
func (s *Server) member(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, status, msg := s.authenticate(r)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		// The team may have changed between authentication and now.
		if _, ok := a.team.Revoked[a.member.ID]; ok {
			writeError(w, http.StatusForbidden, "not a member")
			return
		}
		s.chainNonce(w, r)
		h(w, r, a)
	}
}

// admin is member, for admins only.
func (s *Server) admin(h handler) http.HandlerFunc {
	return s.member(func(w http.ResponseWriter, r *http.Request, a *authed) {
		if !a.member.Admin {
			writeError(w, http.StatusForbidden, "admins only")
			return
		}
		h(w, r, a)
	})
}

// authenticate checks a Fleet-Member request. status is 0 on success.
func (s *Server) authenticate(r *http.Request) (*authed, int, string) {
	id, nonce, sig, ok := team.ParseMemberAuth(r.Header.Get("Authorization"))
	if !ok {
		return nil, http.StatusUnauthorized, "a signed member request is required"
	}
	if !s.nonces.take(nonce) {
		return nil, http.StatusUnauthorized, "nonce unknown or expired"
	}
	limit := int64(maxJSONBody)
	if strings.Contains(r.URL.Path, "/data/") {
		limit = team.MaxBundleData
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge, "request too large"
	}
	teamID := r.PathValue("t")
	s.mu.Lock()
	t, ok := s.teams[teamID]
	var m team.Member
	var revoked bool
	if ok {
		m, ok = t.head().Member(id)
		_, revoked = t.Revoked[id]
	}
	s.mu.Unlock()
	if !ok || revoked {
		// The same answer for an unknown team and a non-member: a stranger
		// learns nothing about which teams exist.
		return nil, http.StatusForbidden, "not a member"
	}
	sum := sha256.Sum256(body)
	if !team.VerifyRequest(m.SignPub, teamID, nonce, r.Method, r.RequestURI, sum[:], sig) {
		return nil, http.StatusUnauthorized, "bad signature"
	}
	return &authed{team: t, member: m, body: body}, 0, ""
}

// readJSON decodes a JSON body, strictly.
func readJSON(body []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// clientIP is the address rate limits go by.
func (s *Server) clientIP(r *http.Request) string {
	if s.opts.TrustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter allows limit requests per IP per rateWindow (a fixed
// window: simple, and enough to blunt guessing and flooding).
type rateLimiter struct {
	mu    sync.Mutex
	start time.Time
	count map[string]int
	limit int
	now   func() time.Time
}

func newRateLimiter(now func() time.Time, limit int) *rateLimiter {
	return &rateLimiter{start: now(), count: map[string]int{}, limit: limit, now: now}
}

func (l *rateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.start) >= rateWindow {
		l.start, l.count = now, map[string]int{}
	}
	l.count[ip]++
	return l.count[ip] <= l.limit
}

// codes are the one-time team creation codes the operator hands out.
type codes struct {
	mu     sync.Mutex
	m      map[string]time.Time // normalized code -> expiry
	failed int
	now    func() time.Time
}

// codeAlphabet is Crockford base32 (no I, L, O, U), as pairing codes use.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeLen is 12 characters, 60 bits: shown as XXXX-XXXX-XXXX.
const codeLen = 12

// maxCodeFailures is how many wrong codes invalidate all live ones.
const maxCodeFailures = 20

// issue makes a code good for codeTTL.
func (c *codes) issue() (string, time.Time) {
	var b [codeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("hub: crypto/rand failed: " + err.Error())
	}
	code := make([]byte, codeLen)
	for i := range code {
		code[i] = codeAlphabet[int(b[i])&31]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]time.Time{}
	}
	exp := c.now().Add(codeTTL)
	for k, e := range c.m {
		if !c.now().Before(e) {
			delete(c.m, k)
		}
	}
	c.m[string(code)] = exp
	return string(code[:4]) + "-" + string(code[4:8]) + "-" + string(code[8:]), exp
}

// redeem consumes code if it is live.
func (c *codes) redeem(code string) bool {
	norm := normalizeCode(code)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.m {
		if subtle.ConstantTimeCompare([]byte(k), []byte(norm)) == 1 && c.now().Before(exp) {
			delete(c.m, k)
			c.failed = 0
			return true
		}
	}
	if c.failed++; c.failed >= maxCodeFailures {
		clear(c.m)
		c.failed = 0
	}
	return false
}

// normalizeCode upper-cases and strips dashes and spaces.
func normalizeCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	return b.String()
}
