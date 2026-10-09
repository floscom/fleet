package web

// Managing other fleets from one dashboard.
//
// Daemons that hold the same fleet key (Options.KeyPath, set on a machine
// with `fleet start --join KEY`) trust each other's dashboards: the browser
// asks its own daemon for /api/hosts/{id}/..., and that daemon signs the
// request with the key and sends it to daemon {id}, found on the LAN via
// mDNS. The key itself never crosses the network.
//
// A signed request is a normal admin request with
//
//	Authorization: Fleet-Key <nonce>.<hex mac>
//
// where the nonce comes from GET /api/nonce on the target (single use,
// nonceTTL) and mac = HMAC-SHA256(key, peerMACLabel "\n" target server id
// "\n" nonce "\n" method "\n" request URI "\n" hex(SHA-256(body))). The
// target checks it against its own server id, so a signature made for one
// daemon is useless on another, and a machine that merely advertises itself
// as a fleet learns nothing it can replay.
//
// Only the browser's admin token reaches /api/hosts/ and /api/join: a
// signed request cannot be forwarded again.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	peerScheme   = "Fleet-Key "
	peerMACLabel = "fleet-web-peer-v1"
	// nonceTTL is how long a nonce from /api/nonce stays usable.
	nonceTTL = 30 * time.Second
	// maxNonces caps outstanding nonces (the endpoint needs no auth).
	maxNonces = 256
	// peerTimeout bounds one request to another daemon (adapter detection
	// alone may take adaptersTimeout); see forwardTimeout.
	peerTimeout     = 15 * time.Second
	peerDialTimeout = 2 * time.Second
	// maxPeerReply caps a reply relayed from another daemon.
	maxPeerReply = 6 << 20 // an image of a transcript line is up to transcript.MaxLine
)

// errNotJoined means the other daemon does not hold our fleet key.
var errNotJoined = errors.New("not in this fleet")

// ValidKey reports whether key looks like a fleet key (or admin token):
// 64 lowercase hex digits.
func ValidKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SetToken stores key at path (mode 0600), replacing what was there.
func SetToken(path, key string) error {
	if !ValidKey(key) {
		return errors.New("a fleet key is 64 hex digits, as printed by `fleet web`")
	}
	if err := writeFileAtomic(path, []byte(key+"\n")); err != nil {
		return fmt.Errorf("write fleet key: %w", err)
	}
	return nil
}

// peerMAC is the signature of one request to daemon targetID.
func peerMAC(key, targetID, nonce, method, uri string, body []byte) []byte {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "%s\n%s\n%s\n%s\n%s\n%s", peerMACLabel, targetID, nonce, method, uri, hex.EncodeToString(sum[:]))
	return m.Sum(nil)
}

// nonces are the outstanding single-use nonces of /api/nonce.
type nonces struct {
	mu sync.Mutex
	m  map[string]time.Time // nonce -> expiry
}

// issue returns a new nonce, or false when too many are outstanding.
func (n *nonces) issue() (string, bool) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.m == nil {
		n.m = map[string]time.Time{}
	}
	if len(n.m) >= maxNonces {
		for k, exp := range n.m {
			if now.After(exp) {
				delete(n.m, k)
			}
		}
		if len(n.m) >= maxNonces {
			return "", false
		}
	}
	nonce := hex.EncodeToString(b[:])
	n.m[nonce] = now.Add(nonceTTL)
	return nonce, true
}

// take consumes nonce and reports whether it was issued and is unexpired.
func (n *nonces) take(nonce string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	exp, ok := n.m[nonce]
	delete(n.m, nonce)
	return ok && time.Now().Before(exp)
}

func (s *Server) apiNonce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.opts.KeyPath == "" {
		writeError(w, http.StatusNotFound, "this fleet does not accept a fleet key")
		return
	}
	nonce, ok := s.nonces.issue()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"nonce": nonce})
}

// isPeerRequest reports whether r claims to be signed with the fleet key.
func isPeerRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), peerScheme)
}

// peerAuthed reports whether r is correctly signed with the fleet key for
// this daemon. r's body must be buffered (it is read and replaced). The
// nonce is spent either way: one guess per nonce.
func (s *Server) peerAuthed(r *http.Request) bool {
	v, _ := strings.CutPrefix(r.Header.Get("Authorization"), peerScheme)
	nonce, macHex, ok := strings.Cut(v, ".")
	if !ok || s.opts.KeyPath == "" || !s.nonces.take(nonce) {
		return false
	}
	key, err := readToken(s.opts.KeyPath)
	if err != nil || key == "" {
		return false
	}
	mac, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return hmac.Equal(mac, peerMAC(key, s.opts.Source.Info().ID, nonce, r.Method, r.RequestURI, body))
}

// apiJoin tells an admin browser the command that joins another machine to
// this fleet, creating the fleet key if needed.
func (s *Server) apiJoin(w http.ResponseWriter, r *http.Request) {
	if s.opts.KeyPath == "" {
		writeError(w, http.StatusNotFound, "this fleet does not accept a fleet key")
		return
	}
	key, err := LoadOrCreateToken(s.opts.KeyPath)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"command": JoinCommand(key)})
}

// JoinCommand is what to run on another machine to join the fleet of key.
func JoinCommand(key string) string { return "fleet start --join " + key }

// forwardable reports whether method on /api/<rest> may be sent to another
// daemon.
func forwardable(method, rest string) bool {
	switch method {
	case http.MethodGet:
		switch rest {
		case "session", "fs", "adapters", "usage", "roots", "agents", "workflows":
			return true
		}
		return agentRoute(rest, "chat", "screen", "workflows", "image", "media") || workflowChatRoute(rest, "chat", "image")
	case http.MethodPost:
		return rest == "roots" || rest == "agents" || agentRoute(rest, "input", "answer", "model", "stop", "merge")
	case http.MethodPut, http.MethodDelete:
		return oneSegment(strings.CutPrefix(rest, "roots/"))
	}
	return false
}

// agentRoute reports whether rest is agents/<id>/<one of actions>.
func agentRoute(rest string, actions ...string) bool {
	id, action, ok := strings.Cut(strings.TrimPrefix(rest, "agents/"), "/")
	if !ok || !strings.HasPrefix(rest, "agents/") || !oneSegment(id, true) {
		return false
	}
	for _, a := range actions {
		if action == a {
			return true
		}
	}
	return false
}

// workflowChatRoute reports whether rest is
// agents/<id>/workflows/<run>/agents/<sub>/<one of actions>.
func workflowChatRoute(rest string, actions ...string) bool {
	p := strings.Split(rest, "/")
	return len(p) == 7 && p[0] == "agents" && p[1] != "" && p[2] == "workflows" && p[3] != "" &&
		p[4] == "agents" && p[5] != "" && slices.Contains(actions, p[6])
}

// oneSegment reports whether s is a non-empty path segment (and ok).
func oneSegment(s string, ok bool) bool {
	return ok && s != "" && !strings.Contains(s, "/")
}

// forwardTimeout bounds a forwarded request: starting an agent may take
// long (a worktree, a container), and so may switching its model; a chat
// request waits up to chatWait.
func forwardTimeout(method, rest string) time.Duration {
	if method == http.MethodPost && rest == "agents" {
		return runTimeout
	}
	if method == http.MethodPost && agentRoute(rest, "model") {
		return modelTimeout
	}
	if method == http.MethodPost && agentRoute(rest, "merge") {
		return mergeTimeout
	}
	return peerTimeout
}

// apiHost serves /api/hosts/{id}/{rest...}: the same request signed with
// the fleet key and sent to the fleet daemon {id} on the LAN. A daemon
// without our key answers 403 with "notJoined".
func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	id, rest := r.PathValue("id"), r.PathValue("rest")
	if !forwardable(r.Method, rest) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.opts.KeyPath == "" {
		writeError(w, http.StatusNotFound, "this fleet does not use a fleet key")
		return
	}
	p, ok := s.hub.peer(id)
	if !ok {
		writeError(w, http.StatusNotFound, "that machine is not on the network (anymore)")
		return
	}
	name := p.Name
	if name == "" {
		name = p.Host
	}
	if p.WebPort == 0 || len(p.Addrs) == 0 {
		writeError(w, http.StatusConflict, name+" has no dashboard on the network (web is off or loopback-only there)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit(r.URL.Path)))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	key, err := LoadOrCreateToken(s.opts.KeyPath)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	// The escaped rest, so a root name like "my%20root" stays one segment.
	uri := "/api/" + rest
	if parts := strings.SplitN(r.URL.EscapedPath(), "/", 5); len(parts) == 5 {
		uri = "/api/" + parts[4]
	}
	if r.URL.RawQuery != "" {
		uri += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout(r.Method, rest))
	defer cancel()
	status, reply, err := s.forward(ctx, p, key, r.Method, uri, body)
	switch {
	case errors.Is(err, errNotJoined) || status == http.StatusUnauthorized:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":     name + " is not in this fleet yet: run `fleet start --join <key>` there",
			"notJoined": true,
		})
	case err != nil:
		s.opts.Log.Debug("web ui: peer request failed", "peer", name, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach "+name)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}
}

// forward sends a signed request to peer p, trying its addresses in order
// until one hands out a nonce. It returns the reply's status and body.
func (s *Server) forward(ctx context.Context, p Peer, key, method, uri string, body []byte) (int, []byte, error) {
	var lastErr error
	for _, a := range p.Addrs {
		base := "http://" + net.JoinHostPort(a, strconv.Itoa(p.WebPort))
		nonce, err := s.peerNonce(ctx, base)
		if err != nil {
			if errors.Is(err, errNotJoined) || ctx.Err() != nil {
				return 0, nil, err
			}
			lastErr = err
			continue // address not reachable from here: try the next one
		}
		req, err := http.NewRequestWithContext(ctx, method, base+uri, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		mac := peerMAC(key, p.ID, nonce, method, req.URL.RequestURI(), body)
		req.Header.Set("Authorization", peerScheme+nonce+"."+hex.EncodeToString(mac))
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		// Not retried on another address: the request may have been applied.
		resp, err := s.peerHTTP.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		reply, err := io.ReadAll(io.LimitReader(resp.Body, maxPeerReply+1))
		if err != nil {
			return 0, nil, err
		}
		if len(reply) > maxPeerReply {
			return 0, nil, errors.New("reply too large")
		}
		if !json.Valid(reply) && resp.StatusCode != http.StatusNoContent {
			return 0, nil, fmt.Errorf("status %d without a JSON reply", resp.StatusCode)
		}
		return resp.StatusCode, reply, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no address")
	}
	return 0, nil, lastErr
}

// peerNonce gets a nonce from the dashboard at base. A daemon without a
// fleet key, or older than fleet keys, gives errNotJoined.
func (s *Server) peerNonce(ctx context.Context, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/nonce", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.peerHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized:
		return "", errNotJoined
	default:
		return "", fmt.Errorf("nonce: status %d", resp.StatusCode)
	}
	var v struct{ Nonce string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v); err != nil || v.Nonce == "" {
		return "", errors.New("nonce: bad reply")
	}
	return v.Nonce, nil
}

// newPeerClient is the HTTP client for other daemons' dashboards: no
// proxy from the environment, no redirects, short dials.
func newPeerClient() *http.Client {
	return &http.Client{
		Timeout: runTimeout, // a backstop: requests carry their own deadline

		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: peerDialTimeout}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
