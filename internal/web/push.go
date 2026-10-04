package web

// Web Push: the dashboard, installed as a web app (on iOS 16.4+ added to
// the Home Screen), subscribes to notifications, and the daemon sends one
// when one of its agents needs input, is done (its turn ended and nothing
// goes on in its background) or fails.
//
// This is the standard Push API, so it works the same with Apple's push
// service (web.push.apple.com), FCM and Mozilla's: the daemon signs each
// request with its VAPID key (RFC 8292) and encrypts the payload for the
// subscription (RFC 8291, aes128gcm). No account or relay of our own is
// involved; the daemon posts straight to the endpoint the browser handed
// out. Browsers only offer push to secure contexts, so the page must be
// served over HTTPS (e.g. `tailscale serve`), not the plain HTTP port.
//
// The VAPID key (PKCS#8 PEM) and the subscriptions (JSON) live in files
// next to the admin token. Subscribing needs the admin token: notifications
// name agents and folders. Endpoints the push service reports gone (404,
// 410) are dropped.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fleet/internal/workflow"
)

const (
	// pushTTL is how long a push service keeps an undelivered message.
	pushTTL = 12 * time.Hour
	// pushTimeout bounds one request to a push service.
	pushTimeout = 15 * time.Second
	// jwtLife is how long a VAPID token is valid (at most a day); one is
	// reused for jwtReuse, as Apple asks for no more than one an hour.
	jwtLife  = 12 * time.Hour
	jwtReuse = 6 * time.Hour
	// pushSubject is the VAPID contact; Apple wants a URL or mailto:.
	pushSubject = "https://github.com/floscom/fleet"
	// maxPushSubs caps the stored subscriptions.
	maxPushSubs = 50
	// recordSize is the aes128gcm record size; payloads fit one record.
	recordSize = 4096
	// maxPushPayload is what fits one record (Apple allows 4 KB).
	maxPushPayload = 3000
	// doneWait is how long an agent must stay idle before "is done" is
	// sent, and how often it is looked at again while its session works
	// in the background (see holdDoneLocked).
	doneWait = 20 * time.Second
	// staleRun is how long a background run may show no activity and
	// still hold back "is done".
	staleRun = 30 * time.Minute
)

var b64 = base64.RawURLEncoding

// PushSub is a browser's push subscription.
type PushSub struct {
	Endpoint string `json:"endpoint"`
	// P256dh is the browser's public key, Auth its auth secret (base64url).
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
	// CreatedAtMs is when it was registered (Unix ms).
	CreatedAtMs int64 `json:"createdAtMs"`
}

// Notice is a notification, as the service worker shows it.
type Notice struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Tag groups notifications: a newer one for the same agent replaces
	// the older one.
	Tag string `json:"tag"`
	// Agent is the agent to open when the notification is tapped.
	Agent string `json:"agent,omitempty"`
}

type cachedJWT struct {
	token string
	made  time.Time
}

// pusher holds the VAPID key and the subscriptions and sends notices.
type pusher struct {
	keyPath, subsPath string
	log               *slog.Logger
	client            *http.Client

	mu   sync.Mutex
	key  *ecdsa.PrivateKey
	subs []PushSub
	// loaded is set once subs was read from subsPath.
	loaded bool
	jwts   map[string]cachedJWT // by audience (push service origin)
}

func newPusher(keyPath, subsPath string, log *slog.Logger) *pusher {
	if keyPath == "" || subsPath == "" {
		return nil
	}
	return &pusher{
		keyPath: keyPath, subsPath: subsPath, log: log,
		client: &http.Client{Timeout: pushTimeout},
		jwts:   map[string]cachedJWT{},
	}
}

// publicKey returns the VAPID public key (uncompressed point, base64url)
// that browsers subscribe with, creating the key pair on first use.
func (p *pusher) publicKey() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key, err := p.keyLocked()
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(pubBytes(key)), nil
}

func pubBytes(key *ecdsa.PrivateKey) []byte {
	return elliptic.Marshal(elliptic.P256(), key.X, key.Y) //nolint:staticcheck // uncompressed point, as VAPID wants
}

// keyLocked loads or creates the VAPID key.
func (p *pusher) keyLocked() (*ecdsa.PrivateKey, error) {
	if p.key != nil {
		return p.key, nil
	}
	b, err := os.ReadFile(p.keyPath)
	switch {
	case err == nil:
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("push key %s: not PEM", p.keyPath)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("push key %s: %w", p.keyPath, err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok || ek.Curve != elliptic.P256() {
			return nil, fmt.Errorf("push key %s: not a P-256 key", p.keyPath)
		}
		p.key = ek
	case errors.Is(err, fs.ErrNotExist):
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(p.keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return nil, fmt.Errorf("write push key: %w", err)
		}
		p.key = k
	default:
		return nil, err
	}
	return p.key, nil
}

// subsLocked returns the subscriptions, reading the file on first use.
func (p *pusher) subsLocked() []PushSub {
	if !p.loaded {
		p.loaded = true
		if b, err := os.ReadFile(p.subsPath); err == nil {
			if err := json.Unmarshal(b, &p.subs); err != nil {
				p.log.Warn("push subscriptions unreadable", "path", p.subsPath, "err", err)
			}
		}
	}
	return p.subs
}

func (p *pusher) saveLocked() error {
	b, err := json.MarshalIndent(nonNil(p.subs), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.subsPath), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p.subsPath, append(b, '\n'))
}

// add stores sub, replacing one with the same endpoint.
func (p *pusher) add(sub PushSub) error {
	if err := checkSub(sub); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	subs := p.subsLocked()
	out := []PushSub{sub}
	for _, s := range subs {
		if s.Endpoint != sub.Endpoint && len(out) < maxPushSubs {
			out = append(out, s)
		}
	}
	p.subs = out
	return p.saveLocked()
}

// remove drops the subscription with endpoint; it reports whether there
// was one.
func (p *pusher) remove(endpoint string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	subs := p.subsLocked()
	out := subs[:0:0]
	for _, s := range subs {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	if len(out) == len(subs) {
		return false, nil
	}
	p.subs = out
	return true, p.saveLocked()
}

// has reports whether endpoint is subscribed.
func (p *pusher) has(endpoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.subsLocked() {
		if s.Endpoint == endpoint {
			return true
		}
	}
	return false
}

// checkSub rejects a subscription that cannot be pushed to: the endpoint
// must be https, the keys a P-256 point and a 16-byte secret.
func checkSub(sub PushSub) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("endpoint must be an https URL")
	}
	if pub, err := b64.DecodeString(strings.TrimRight(sub.P256dh, "=")); err != nil || len(pub) != 65 {
		return errors.New("p256dh must be a P-256 public key")
	} else if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("p256dh must be a P-256 public key")
	}
	if auth, err := b64.DecodeString(strings.TrimRight(sub.Auth, "=")); err != nil || len(auth) != 16 {
		return errors.New("auth must be 16 bytes")
	}
	return nil
}

// notify sends n to every subscription in the background.
func (p *pusher) notify(n Notice) {
	if p == nil {
		return
	}
	p.mu.Lock()
	subs := append([]PushSub(nil), p.subsLocked()...)
	p.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(n)
	if err != nil || len(payload) > maxPushPayload {
		p.log.Warn("push: notice too large", "tag", n.Tag)
		return
	}
	for _, s := range subs {
		go func(s PushSub) {
			if err := p.send(context.Background(), s, payload, n.Tag); err != nil {
				p.log.Warn("push failed", "endpoint", endpointHost(s.Endpoint), "err", err)
			}
		}(s)
	}
}

// errGone is a subscription the push service no longer knows.
var errGone = errors.New("subscription gone")

// send posts one encrypted message to sub. A gone subscription is
// removed.
func (p *pusher) send(ctx context.Context, sub PushSub, payload []byte, topic string) error {
	uaPub, err := b64.DecodeString(strings.TrimRight(sub.P256dh, "="))
	if err != nil {
		return err
	}
	auth, err := b64.DecodeString(strings.TrimRight(sub.Auth, "="))
	if err != nil {
		return err
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	body, err := encryptPush(payload, uaPub, auth, asKey, salt)
	if err != nil {
		return err
	}
	authz, err := p.authorization(sub.Endpoint)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(pushTTL/time.Second)))
	req.Header.Set("Urgency", "high")
	if topic != "" {
		req.Header.Set("Topic", pushTopic(topic))
	}
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		if _, err := p.remove(sub.Endpoint); err != nil {
			p.log.Warn("push: drop subscription", "err", err)
		}
		return fmt.Errorf("%w: %s: %s", errGone, res.Status, strings.TrimSpace(string(msg)))
	default:
		return fmt.Errorf("push service: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
}

// pushTopic maps a tag to a Topic header: at most 32 base64url characters.
func pushTopic(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return b64.EncodeToString(sum[:])[:32]
}

func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return ""
}

// authorization returns the VAPID Authorization header for endpoint,
// reusing a token for the same push service for a while.
func (p *pusher) authorization(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	aud := u.Scheme + "://" + u.Host
	p.mu.Lock()
	defer p.mu.Unlock()
	key, err := p.keyLocked()
	if err != nil {
		return "", err
	}
	now := time.Now()
	c, ok := p.jwts[aud]
	if !ok || now.Sub(c.made) > jwtReuse {
		tok, err := vapidJWT(key, aud, now.Add(jwtLife))
		if err != nil {
			return "", err
		}
		c = cachedJWT{token: tok, made: now}
		p.jwts[aud] = c
	}
	return "vapid t=" + c.token + ", k=" + b64.EncodeToString(pubBytes(key)), nil
}

// vapidJWT signs the VAPID claims (RFC 8292) with ES256.
func vapidJWT(key *ecdsa.PrivateKey, aud string, exp time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": pushSubject})
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	// JWS wants r||s, each padded to 32 bytes, not ASN.1.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// verifyVAPID checks a token from vapidJWT; tests use it.
func verifyVAPID(pub *ecdsa.PublicKey, token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return false
	}
	sig, err := b64.DecodeString(token[i+1:])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(token[:i]))
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

// encryptPush encrypts payload for a subscription (RFC 8291): one
// aes128gcm record (RFC 8188) with the server's ephemeral key asKey and
// salt in its header.
func encryptPush(payload, uaPub, auth []byte, asKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	secret, err := asKey.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := asKey.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm := hkdf(auth, secret, keyInfo, 32)
	cek := hkdf(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdf(salt, ikm, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The 0x02 delimiter marks the last (only) record; no padding.
	plain := append(append([]byte(nil), payload...), 2)
	if len(plain)+gcm.Overhead() > recordSize {
		return nil, errors.New("push payload too large")
	}
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPub)))
	header = append(header, asPub...)
	return gcm.Seal(header, nonce, plain, nil), nil
}

// hkdf is HKDF-SHA256 (RFC 5869) for outputs of at most 32 bytes: one
// expand block.
func hkdf(salt, ikm, info []byte, n int) []byte {
	ext := hmac.New(sha256.New, salt)
	ext.Write(ikm)
	exp := hmac.New(sha256.New, ext.Sum(nil))
	exp.Write(info)
	exp.Write([]byte{1})
	return exp.Sum(nil)[:n]
}

// ---------------------------------------------------------------------------
// Notices

// noticeLocked tells the user about a's change from prev (see notice and
// limitNotice). "Needs you", "failed" and usage limits go out at once; "is
// done" is held (holdDoneLocked), and any later change of the agent's
// state drops it. An agent a usage limit stopped is not done.
func (h *hub) noticeLocked(prev agentEntry, a Agent) {
	if a.State != prev.state || a.UsageLimit != nil {
		h.dropDoneLocked(a.ID)
	}
	if n, ok := limitNotice(prev.limit, a); ok {
		h.notify(n)
		return
	}
	n, ok := notice(prev.state, a)
	switch {
	case !ok:
	case a.State == "idle":
		if a.UsageLimit == nil {
			h.holdDoneLocked(a.ID, n)
		}
	default:
		h.notify(n)
	}
}

// holdDoneLocked sends n, the "is done" of agent id, once the agent stayed
// idle for doneWait and its session runs nothing in the background. A
// turn's end is not the end of the work: a background subagent or workflow
// that ends starts the next turn, and so does a background shell command
// (not looked for: dev servers never end, but most commands end soon).
func (h *hub) holdDoneLocked(id string, n Notice) {
	var t *time.Timer
	t = time.AfterFunc(h.doneWait, func() {
		h.mu.Lock()
		due := h.done[id] == t && !h.closed && h.agents[id].state == "idle"
		h.mu.Unlock()
		if !due {
			return
		}
		busy := h.backgroundWork(id)
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case h.done[id] != t || h.closed:
			// Changed while looking.
		case busy:
			h.holdDoneLocked(id, n)
		default:
			delete(h.done, id)
			h.notify(n)
		}
	})
	h.done[id] = t
}

func (h *hub) dropDoneLocked(id string) {
	if t, ok := h.done[id]; ok {
		t.Stop()
		delete(h.done, id)
	}
}

// backgroundWork reports whether the session of agent id runs subagents or
// workflows in the background. Runs quiet for staleRun are not counted:
// one whose agents died with the session never ends.
func (h *hub) backgroundWork(id string) bool {
	_, runs, err := h.src.Workflows(id)
	if err != nil {
		return false
	}
	since := time.Now().Add(-staleRun).UnixMilli()
	for _, r := range runs {
		if r.Status == workflow.Running && max(r.UpdatedMs, r.StartedMs) >= since {
			return true
		}
	}
	return false
}

// notice says what to tell the user when an agent goes from state prev to
// a.State: it needs input, finished its turn, or failed.
func notice(prev string, a Agent) (Notice, bool) {
	if prev == a.State {
		return Notice{}, false
	}
	name := a.Name
	if name == "" {
		name = a.Adapter
	}
	where := noticeWhere(a)
	n := Notice{Tag: "agent:" + a.ID, Agent: a.ID}
	switch {
	case a.State == "needs_input":
		n.Title = name + " needs you"
		n.Body = firstNonEmpty(a.StateDetail, "Waiting for your answer")
	case a.State == "idle" && (prev == "working" || prev == "running"):
		n.Title = name + " is done"
		n.Body = firstNonEmpty(a.StateDetail, "Finished its turn")
	case a.State == "failed":
		n.Title = name + " failed"
		n.Body = firstNonEmpty(a.StateDetail, "The agent stopped")
	default:
		return Notice{}, false
	}
	if where != "" {
		n.Body = where + " · " + n.Body
	}
	if len(n.Body) > 300 {
		n.Body = n.Body[:300] + "…"
	}
	return n, true
}

// noticeWhere names where agent a works, for a notice.
func noticeWhere(a Agent) string {
	where := filepath.Base(a.Cwd)
	if where == "." || where == "/" {
		where = a.Root
	}
	return where
}

// limitNotice says what to tell the user when a usage limit stops agent a
// (prev: what stopped it before, nil if nothing), or auto-resume gives up.
func limitNotice(prev *AgentLimit, a Agent) (Notice, bool) {
	l := a.UsageLimit
	if l == nil || (prev != nil && (l.Detail == "" || l.Detail == prev.Detail)) {
		return Notice{}, false
	}
	n := Notice{Tag: "agent:" + a.ID, Agent: a.ID, Title: firstNonEmpty(a.Name, a.Adapter) + " hit a usage limit"}
	var parts []string
	if where := noticeWhere(a); where != "" {
		parts = append(parts, where)
	}
	if l.Window != "" {
		parts = append(parts, l.Window+" limit")
	}
	switch {
	case l.Detail != "":
		parts = append(parts, l.Detail)
	case l.ResumeAtMs > 0:
		parts = append(parts, "resumes "+clock(time.UnixMilli(l.ResumeAtMs)))
	case l.ResetsAtMs > 0:
		parts = append(parts, "resets "+clock(time.UnixMilli(l.ResetsAtMs))+", auto-resume off")
	default:
		parts = append(parts, "auto-resume off")
	}
	n.Body = strings.Join(parts, " · ")
	return n, true
}

// clock is t as the time of day, with the weekday if not today.
func clock(t time.Time) string {
	if y, m, d := t.Date(); y == time.Now().Year() && m == time.Now().Month() && d == time.Now().Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Routes

// pushOff answers 404 when push is disabled and reports whether it did.
func (s *Server) pushOff(w http.ResponseWriter) bool {
	if s.push == nil {
		writeError(w, http.StatusNotFound, "notifications are off on this daemon")
		return true
	}
	return false
}

// apiPushKey returns the VAPID public key and, for ?endpoint=, whether
// that subscription is known here.
func (s *Server) apiPushKey(w http.ResponseWriter, r *http.Request) {
	if s.pushOff(w) {
		return
	}
	key, err := s.push.publicKey()
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	ep := r.URL.Query().Get("endpoint")
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "subscribed": ep != "" && s.push.has(ep)})
}

type pushSubRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// apiPushSubscribe stores a PushSubscription (its toJSON() form).
func (s *Server) apiPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.pushOff(w) {
		return
	}
	var req pushSubRequest
	if !decode(w, r, &req) {
		return
	}
	sub := PushSub{Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth, CreatedAtMs: time.Now().UnixMilli()}
	if err := s.push.add(sub); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiPushUnsubscribe forgets a subscription.
func (s *Server) apiPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.pushOff(w) {
		return
	}
	var req pushSubRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.push.remove(req.Endpoint); err != nil {
		s.writeSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiPushTest sends a test notification to one subscription and reports
// the push service's answer.
func (s *Server) apiPushTest(w http.ResponseWriter, r *http.Request) {
	if s.pushOff(w) {
		return
	}
	var req pushSubRequest
	if !decode(w, r, &req) {
		return
	}
	var sub PushSub
	s.push.mu.Lock()
	for _, v := range s.push.subsLocked() {
		if v.Endpoint == req.Endpoint {
			sub = v
		}
	}
	s.push.mu.Unlock()
	if sub.Endpoint == "" {
		writeError(w, http.StatusNotFound, "this browser is not subscribed")
		return
	}
	name := s.opts.Source.Info().Name
	payload, _ := json.Marshal(Notice{Title: "fleet · " + name, Body: "Notifications work.", Tag: "test"})
	if err := s.push.send(r.Context(), sub, payload, "test"); errors.Is(err, errGone) {
		writeError(w, http.StatusGone, err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
