// Package hub is the team hub server (`fleet hub`): a store-and-forward
// service that holds, per team, the signed roster chain, the sealed team
// keys, pending invites and join requests, bundle heads with their opaque
// data blobs, and an event log that members long-poll. It never relays
// requests to a daemon.
//
// The hub sees ids, public keys, sizes and times, and nothing else: names
// and skill contents reach it sealed (package team). It still checks what
// it can, as a second line behind the clients' own checks: every request
// is signed by a member of the team's current roster, a new roster must
// follow the chain rules (team.VerifyNext), a bundle head must be signed
// by its owner and its version must rise, and a member removed by a roster
// or leaving on their own is cut off at once and their bundles dropped.
//
// State lives in files under the data directory (0700):
//
//	operator.json          the operator token and the loopback address
//	                       `fleet hub code` talks to
//	teams/<id>/team.json   one team's index, written atomically on change
//	teams/<id>/data/<ref>  bundle data, content-addressed
//	autocert/              TLS certificates (--autocert)
//
// Limits keep one team or one client from taking the hub down: request
// bodies are capped, bundle data at team.MaxBundleData, a team's data at
// Options.Quota, members at team.MaxMembers, and the unauthenticated
// endpoints (nonce, create, join) are rate limited per IP.
package hub

import (
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// maxJSONBody caps a JSON request: a full roster of team.MaxMembers
	// members with their KEM keys is about half of this.
	maxJSONBody = 2 << 20
	// nonceTTL is how long a nonce stays usable; maxNonces caps how many
	// may be outstanding in all, maxNoncesPerIP per client address, so
	// one address cannot starve the others (the endpoint needs no auth).
	nonceTTL       = 30 * time.Second
	maxNonces      = 8192
	maxNoncesPerIP = 256
	// pendingTTL is how long an uploaded blob waits for a head to reference
	// it before it is deleted.
	pendingTTL = time.Hour
	// maxInvites and maxJoins cap what admins and joiners can queue up per
	// team.
	maxInvites    = 64
	maxJoins      = 64
	maxInviteTTL  = 30 * 24 * time.Hour
	maxNameSealed = 512
	// maxEvents is how many events a team's log keeps; a client whose
	// cursor is older gets Reset and fetches everything afresh.
	maxEvents = 2000
	// codeTTL is how long a team creation code is good for.
	codeTTL = time.Hour
	// rateWindow is the window of the per-IP rate limit on unauthenticated
	// requests.
	rateWindow = time.Minute
)

// NonceHeader is the response header of every signed request: a fresh
// nonce for the client's next one, so a member needs GET /v1/nonce only
// for its first request (and after a pause longer than nonceTTL).
const NonceHeader = "Fleet-Nonce"

// Variables so tests can shorten them.
var (
	// rateLimit is the number of unauthenticated requests (create, join)
	// one IP may make per rateWindow.
	rateLimit = 60
	// nonceRateLimit is the same for GET /v1/nonce: far higher, as every
	// member behind one address needs one to start with, and a nonce is
	// cheap and bounded by maxNoncesPerIP and nonceTTL anyway.
	nonceRateLimit = 600
	// pollWait is how long an events request waits for something to happen.
	pollWait = 25 * time.Second
)

// DefaultQuota is the default per-team data quota.
const DefaultQuota = 1 << 30

// Options configure a Server.
type Options struct {
	// DataDir holds all state (created 0700).
	DataDir string
	// Open lets anyone create a team, without a creation code.
	Open bool
	// Quota is the per-team cap on stored bundle data in bytes (0 =
	// DefaultQuota).
	Quota int64
	// TrustForwarded takes the client IP from X-Forwarded-For (behind a
	// reverse proxy that sets it).
	TrustForwarded bool
	Log            *slog.Logger
	// now is the clock; tests set it.
	now func() time.Time
}

// Server is the hub: an http.Handler plus its state. New loads the state;
// Serve (serve.go) runs it with listeners.
type Server struct {
	opts  Options
	log   *slog.Logger
	mux   *http.ServeMux
	store *store

	mu    sync.Mutex
	teams map[string]*teamState

	nonces nonces
	codes  codes
	// limiter bounds the unauthenticated endpoints, nonceLimiter GET
	// /v1/nonce.
	limiter      *rateLimiter
	nonceLimiter *rateLimiter
}

// New loads (or starts) a hub in opts.DataDir.
func New(opts Options) (*Server, error) {
	if opts.Quota <= 0 {
		opts.Quota = DefaultQuota
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	st, err := openStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	teams, err := st.loadTeams()
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, log: opts.Log, store: st, teams: teams,
		limiter: newRateLimiter(opts.now, rateLimit), nonceLimiter: newRateLimiter(opts.now, nonceRateLimit)}
	s.nonces.now, s.codes.now = opts.now, opts.now
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

// ServeHTTP serves the public API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /v1/nonce", s.handleNonce)
	m.HandleFunc("POST /v1/teams", s.handleCreate)
	m.HandleFunc("GET /v1/teams/{t}/roster", s.member(s.handleRosters))
	m.HandleFunc("POST /v1/teams/{t}/roster", s.admin(s.handlePostRoster))
	m.HandleFunc("GET /v1/teams/{t}/keys", s.member(s.handleKeys))
	m.HandleFunc("POST /v1/teams/{t}/keys", s.admin(s.handlePostKeys))
	m.HandleFunc("POST /v1/teams/{t}/invites", s.admin(s.handlePostInvite))
	m.HandleFunc("POST /v1/teams/{t}/joins", s.handleJoin)
	m.HandleFunc("GET /v1/teams/{t}/joins", s.admin(s.handleJoins))
	m.HandleFunc("DELETE /v1/teams/{t}/joins/{id}", s.admin(s.handleDeleteJoin))
	m.HandleFunc("POST /v1/teams/{t}/leave", s.member(s.handleLeave))
	m.HandleFunc("GET /v1/teams/{t}/bundles", s.member(s.handleHeads))
	m.HandleFunc("GET /v1/teams/{t}/bundles/{b}", s.member(s.handleHead))
	m.HandleFunc("PUT /v1/teams/{t}/bundles/{b}", s.member(s.handlePutHead))
	m.HandleFunc("PUT /v1/teams/{t}/data/{ref}", s.member(s.handlePutData))
	m.HandleFunc("GET /v1/teams/{t}/data/{ref}", s.member(s.handleData))
	m.HandleFunc("GET /v1/teams/{t}/events", s.member(s.handleEvents))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
}
