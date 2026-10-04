package web

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/workflow"
)

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("base64 %q: %v", s, err)
	}
	return b
}

// TestEncryptPushRFC8291 checks the encryption against the example in RFC
// 8291 (section 5 and appendix A).
func TestEncryptPushRFC8291(t *testing.T) {
	asKey, err := ecdh.P256().NewPrivateKey(unb64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPush(
		[]byte("When I grow up, I want to be a watermelon"),
		unb64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		unb64(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		asKey,
		unb64(t, "DGv6ra1nlYgDCS1FRnbzlw"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// The 86-byte header, then the ciphertext.
	want := append(unb64(t, `DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml
		mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8`),
		unb64(t, `8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ`)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("encrypted:\n got %s\nwant %s", b64.EncodeToString(got), b64.EncodeToString(want))
	}
}

// decryptPush is the browser's side of encryptPush.
func decryptPush(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(body) < 86 || binary.BigEndian.Uint32(body[16:20]) != recordSize || body[20] != 65 {
		t.Fatalf("bad header: % x", body[:min(len(body), 21)])
	}
	salt, asPub, ct := body[:16], body[21:86], body[86:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ua.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)
	ikm := hkdf(auth, secret, keyInfo, 32)
	block, _ := aes.NewCipher(hkdf(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), 16))
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, hkdf(salt, ikm, []byte("Content-Encoding: nonce\x00"), 12), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain[len(plain)-1] != 2 {
		t.Fatalf("no last-record delimiter")
	}
	return plain[:len(plain)-1]
}

type pushed struct {
	header http.Header
	body   []byte
}

// TestPushFlow subscribes a browser through the API, moves an agent from
// working to idle and checks what reaches the push service; a 410 from
// the service drops the subscription.
func TestPushFlow(t *testing.T) {
	got := make(chan pushed, 8)
	var status atomic.Int32
	status.Store(http.StatusCreated)
	svc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- pushed{r.Header.Clone(), b}
		w.WriteHeader(int(status.Load()))
	}))
	defer svc.Close()

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "web-token")
	tok, err := LoadOrCreateToken(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	working := &fleetv1.Agent{Id: "a1", Name: "claude-1", Adapter: "claude", Cwd: "/home/flo/fleet", State: fleetv1.AgentState_AGENT_STATE_WORKING}
	src := newFakeSource(working)
	s, err := New(Options{
		Addr: "127.0.0.1:0", Source: src, TokenPath: tokPath,
		PushKeyPath: filepath.Join(dir, "push-key"), PushSubsPath: filepath.Join(dir, "push.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.push.client = svc.Client()
	s.hub.doneWait = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s.start(ctx)
	ts := httptest.NewServer(s.handler())
	defer func() { cancel(); s.wg.Wait(); ts.Close() }()

	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	code, out := call("GET", "/api/push", nil)
	key, _ := out["key"].(string)
	if code != 200 || len(unb64(t, key)) != 65 {
		t.Fatalf("GET /api/push: %d %v", code, out)
	}

	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	endpoint := svc.URL + "/push/abc"
	sub := map[string]any{"endpoint": endpoint, "keys": map[string]string{
		"p256dh": b64.EncodeToString(ua.PublicKey().Bytes()), "auth": b64.EncodeToString(auth),
	}}
	if code, out := call("POST", "/api/push/subscribe", map[string]any{"endpoint": "http://plain/x", "keys": sub["keys"]}); code != 400 {
		t.Fatalf("plain http endpoint: %d %v", code, out)
	}
	if code, out := call("POST", "/api/push/subscribe", sub); code != 204 {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	if _, out := call("GET", "/api/push?endpoint="+endpoint, nil); out["subscribed"] != true {
		t.Fatalf("not subscribed: %v", out)
	}

	<-s.hub.ready
	idle := &fleetv1.Agent{Id: "a1", Name: "claude-1", Adapter: "claude", Cwd: "/home/flo/fleet", State: fleetv1.AgentState_AGENT_STATE_IDLE}
	src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: idle}}

	var p pushed
	select {
	case p = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
	}
	if p.header.Get("Content-Encoding") != "aes128gcm" || p.header.Get("Urgency") != "high" || p.header.Get("TTL") == "" || len(p.header.Get("Topic")) != 32 {
		t.Fatalf("headers: %v", p.header)
	}
	// Authorization: vapid t=<jwt>, k=<the key the browser subscribed with>
	authz := p.header.Get("Authorization")
	tokPart, kPart, ok := strings.Cut(strings.TrimPrefix(authz, "vapid t="), ", k=")
	if !ok || kPart != key {
		t.Fatalf("Authorization = %q", authz)
	}
	pub := unb64(t, key)
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	if !verifyVAPID(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, tokPart) {
		t.Fatal("VAPID signature does not verify")
	}
	var claims struct {
		Aud string
		Exp int64
		Sub string
	}
	json.Unmarshal(unb64(t, strings.Split(tokPart, ".")[1]), &claims)
	if claims.Aud != svc.URL || claims.Sub != pushSubject || claims.Exp <= time.Now().Unix() || claims.Exp > time.Now().Add(24*time.Hour).Unix() {
		t.Fatalf("claims: %+v", claims)
	}
	var n Notice
	if err := json.Unmarshal(decryptPush(t, p.body, ua, auth), &n); err != nil {
		t.Fatal(err)
	}
	if n.Title != "claude-1 is done" || n.Agent != "a1" || n.Tag != "agent:a1" || !strings.HasPrefix(n.Body, "fleet · ") {
		t.Fatalf("notice: %+v", n)
	}

	// The same state again sends nothing; the push service saying the
	// subscription is gone drops it.
	status.Store(http.StatusGone)
	src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: idle}}
	needs := &fleetv1.Agent{Id: "a1", Name: "claude-1", State: fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT, StateDetail: "Allow Bash?"}
	src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: needs}}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no push for needs_input")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.push.has(endpoint) {
		if time.Now().After(deadline) {
			t.Fatal("gone subscription kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case p := <-got:
		t.Fatalf("unexpected extra push: %v", p.header)
	default:
	}
}

// TestHoldDone: "is done" waits until the agent stayed idle and its
// session runs nothing in the background; "needs you" does not wait.
func TestHoldDone(t *testing.T) {
	agent := func(state fleetv1.AgentState) *fleetv1.Agent {
		return &fleetv1.Agent{Id: "a1", Name: "claude-1", State: state}
	}
	src := newFakeSource(agent(fleetv1.AgentState_AGENT_STATE_WORKING))
	src.agent = agent(fleetv1.AgentState_AGENT_STATE_WORKING)
	h := newHub(src, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.doneWait = 50 * time.Millisecond
	got := make(chan Notice, 8)
	h.notify = func(n Notice) { got <- n }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)
	<-h.ready

	set := func(state fleetv1.AgentState) {
		src.events <- &fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: agent(state)}}
	}
	expect := func(title string) {
		t.Helper()
		select {
		case n := <-got:
			if n.Title != title {
				t.Fatalf("got %q, want %q", n.Title, title)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q", title)
		}
	}
	quiet := func(d time.Duration) {
		t.Helper()
		select {
		case n := <-got:
			t.Fatalf("unexpected %q", n.Title)
		case <-time.After(d):
		}
	}

	// A turn that ends and another that starts soon after: nothing.
	set(fleetv1.AgentState_AGENT_STATE_IDLE)
	set(fleetv1.AgentState_AGENT_STATE_WORKING)
	quiet(150 * time.Millisecond)

	// A dialog is told at once.
	set(fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT)
	expect("claude-1 needs you")
	set(fleetv1.AgentState_AGENT_STATE_WORKING)

	// Idle while a background workflow runs: held until it ended.
	now := time.Now().UnixMilli()
	src.mu.Lock()
	src.wfRuns = []workflow.Run{{ID: "wf_1", Status: workflow.Running, StartedMs: now, UpdatedMs: now}}
	src.mu.Unlock()
	set(fleetv1.AgentState_AGENT_STATE_IDLE)
	quiet(200 * time.Millisecond)
	src.mu.Lock()
	src.wfRuns[0].Status = workflow.Completed
	src.mu.Unlock()
	expect("claude-1 is done")

	// A run quiet for long does not hold it back.
	set(fleetv1.AgentState_AGENT_STATE_WORKING)
	src.mu.Lock()
	old := time.Now().Add(-2 * staleRun).UnixMilli()
	src.wfRuns = []workflow.Run{{ID: "wf_2", Status: workflow.Running, StartedMs: old, UpdatedMs: old}}
	src.mu.Unlock()
	set(fleetv1.AgentState_AGENT_STATE_IDLE)
	expect("claude-1 is done")
	quiet(150 * time.Millisecond)
}

func TestNotice(t *testing.T) {
	for _, c := range []struct {
		prev, state string
		title       string
	}{
		{"working", "needs_input", "x needs you"},
		{"idle", "needs_input", "x needs you"},
		{"working", "idle", "x is done"},
		{"starting", "idle", ""},
		{"needs_input", "working", ""},
		{"working", "failed", "x failed"},
		{"working", "exited", ""},
		{"idle", "idle", ""},
	} {
		n, ok := notice(c.prev, Agent{Name: "x", State: c.state})
		if ok != (c.title != "") || n.Title != c.title {
			t.Errorf("%s -> %s: %q %v, want %q", c.prev, c.state, n.Title, ok, c.title)
		}
	}
}
