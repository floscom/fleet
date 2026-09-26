package identity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreateServer(dir, "box")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"server.key", "server.crt"} {
		fi, err := os.Stat(filepath.Join(dir, "identity", f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v, %v", f, fi.Mode(), err)
		}
	}
	if len(s.Fingerprint) != 32 || s.ID != hex.EncodeToString(s.Fingerprint) {
		t.Fatalf("bad id/fingerprint: %q", s.ID)
	}
	leaf, err := x509.ParseCertificate(s.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("key type %T", leaf.PublicKey)
	}
	if leaf.Subject.CommonName != "box" {
		t.Fatalf("CN = %q", leaf.Subject.CommonName)
	}
	if leaf.NotAfter.Before(time.Now().AddDate(9, 11, 0)) {
		t.Fatalf("NotAfter too soon: %v", leaf.NotAfter)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("::1"); err != nil {
		t.Fatal(err)
	}

	again, err := LoadOrCreateServer(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != s.ID {
		t.Fatal("identity changed on reload")
	}

	os.Remove(filepath.Join(dir, "identity", "server.crt"))
	if _, err := LoadOrCreateServer(dir, "box"); err == nil {
		t.Fatal("expected error for incomplete identity")
	}
}

func TestTLSHandshakeFingerprint(t *testing.T) {
	s, err := LoadOrCreateServer(t.TempDir(), "box")
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatal("TLS 1.3 minimum not set")
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := CertFingerprint(c.ConnectionState().PeerCertificates[0].Raw)
	if !bytes.Equal(got, s.Fingerprint) {
		t.Fatal("observed fingerprint differs")
	}
}

func TestDeviceIdentityAndAuth(t *testing.T) {
	dir := t.TempDir()
	d, err := LoadOrCreateDevice(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "identity", "device.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("device.key mode %v, %v", fi.Mode(), err)
	}
	if len(d.ID) != 32 || d.ID != DeviceID(d.Public) {
		t.Fatalf("device id %q", d.ID)
	}
	d2, err := LoadOrCreateDevice(dir)
	if err != nil || d2.ID != d.ID || !d2.Private.Equal(d.Private) {
		t.Fatalf("reload mismatch: %v", err)
	}

	nonce := make([]byte, 32)
	rand.Read(nonce)
	fp := bytes.Repeat([]byte{7}, 32)
	sig := d.Sign(nonce, fp)
	if !VerifyAuth(d.Public, nonce, fp, sig) {
		t.Fatal("valid signature rejected")
	}
	if !ed25519.Verify(d.Public, append(append([]byte("fleet-auth-v1"), nonce...), fp...), sig) {
		t.Fatal("signature not over documented message")
	}
	other := bytes.Repeat([]byte{8}, 32)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]bool{
		"wrong nonce": VerifyAuth(d.Public, other, fp, sig),
		"wrong fp":    VerifyAuth(d.Public, nonce, other, sig),
		"wrong key":   VerifyAuth(otherPub, nonce, fp, sig),
		"short key":   VerifyAuth(d.Public[:10], nonce, fp, sig),
		"bad sig":     VerifyAuth(d.Public, nonce, fp, sig[:10]),
	}
	for name, ok := range cases {
		if ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ABCD-EFGH", "ABCDEFGH"},
		{"abcd efgh", "ABCDEFGH"},
		{"oOiI-lL01", "00111101"},
		{"ABCD-EFG", ""},
		{"ABCD-EFGHJ", ""},
		{"ABCD-EFGU", ""}, // U is not Crockford
		{"ABCD_EFGH", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeCode(tt.in); got != tt.want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// book returns a PairingCodes with a controllable clock.
func book() (*PairingCodes, *time.Time) {
	p := NewPairingCodes()
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	return p, &now
}

var (
	fp  = bytes.Repeat([]byte{1}, 32)
	pub = bytes.Repeat([]byte{2}, 32)
)

func TestPairingRoundTrip(t *testing.T) {
	p, _ := book()
	display, exp := p.Issue(time.Minute)
	if len(display) != 9 || display[4] != '-' || NormalizeCode(display) == "" {
		t.Fatalf("display %q", display)
	}
	if exp.IsZero() {
		t.Fatal("zero expiry")
	}
	// The user may type it lower-case with look-alikes; proof uses the normalized code.
	typed := strings.ToLower(display)
	proof := PairProof(typed, fp, pub)
	code, ok := p.Redeem(proof, fp, pub)
	if !ok || code != NormalizeCode(display) {
		t.Fatalf("Redeem = %q, %v", code, ok)
	}
	// Server proof differs from client proof and both sides agree on it.
	sp := ServerPairProof(code, fp, pub)
	if bytes.Equal(sp, proof) || !bytes.Equal(sp, ServerPairProof(display, fp, pub)) {
		t.Fatal("server proof mismatch")
	}
	// Single use.
	if _, ok := p.Redeem(proof, fp, pub); ok {
		t.Fatal("code redeemed twice")
	}
}

func TestPairingProofBindsInputs(t *testing.T) {
	p, _ := book()
	display, _ := p.Issue(time.Minute)
	other := bytes.Repeat([]byte{9}, 32)
	if _, ok := p.Redeem(PairProof(display, other, pub), fp, pub); ok {
		t.Fatal("proof for other server fingerprint accepted")
	}
	if _, ok := p.Redeem(PairProof(display, fp, other), fp, pub); ok {
		t.Fatal("proof for other device key accepted")
	}
	if _, ok := p.Redeem(PairProof(display, fp, pub), fp, pub); !ok {
		t.Fatal("correct proof rejected")
	}
}

func TestPairingWrongCodeAndExhaustion(t *testing.T) {
	p, _ := book()
	display, _ := p.Issue(time.Minute)
	wrong := PairProof("0000-0000", fp, pub)
	if NormalizeCode(display) == "00000000" {
		t.Skip("astronomically unlikely collision")
	}
	for i := 0; i < maxFailedAttempts-1; i++ {
		if _, ok := p.Redeem(wrong, fp, pub); ok {
			t.Fatal("wrong code accepted")
		}
	}
	// Still live after 4 failures.
	if _, ok := p.Redeem(PairProof(display, fp, pub), fp, pub); !ok {
		t.Fatal("code invalidated too early")
	}

	display, _ = p.Issue(time.Minute)
	for i := 0; i < maxFailedAttempts; i++ {
		p.Redeem(wrong, fp, pub)
	}
	if _, ok := p.Redeem(PairProof(display, fp, pub), fp, pub); ok {
		t.Fatal("code valid after 5 failed attempts")
	}
}

func TestPairingNewCodeInvalidatesOld(t *testing.T) {
	p, _ := book()
	old, _ := p.Issue(time.Minute)
	cur, _ := p.Issue(time.Minute)
	if _, ok := p.Redeem(PairProof(old, fp, pub), fp, pub); ok && NormalizeCode(old) != NormalizeCode(cur) {
		t.Fatal("superseded code accepted")
	}
	if _, ok := p.Redeem(PairProof(cur, fp, pub), fp, pub); !ok {
		t.Fatal("current code rejected")
	}
}

func TestPairingExpiry(t *testing.T) {
	p, now := book()
	display, exp := p.Issue(time.Minute)
	if !exp.Equal(now.Add(time.Minute)) {
		t.Fatalf("expiry %v", exp)
	}
	*now = now.Add(time.Minute)
	if _, ok := p.Redeem(PairProof(display, fp, pub), fp, pub); ok {
		t.Fatal("expired code accepted")
	}
}

func TestPairingConcurrentSingleUse(t *testing.T) {
	p := NewPairingCodes()
	display, _ := p.Issue(time.Minute)
	proof := PairProof(display, fp, pub)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := p.Redeem(proof, fp, pub); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("redeemed %d times", wins)
	}
}

func TestRandomCodeAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := randomCode()
		if len(c) != codeLen || NormalizeCode(c) != c {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 195 {
		t.Fatalf("codes not random enough: %d unique", len(seen))
	}
}

func TestDeviceStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "devices.json")
	s, err := OpenDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	must(t, s.Add(PairedDevice{ID: "b", Name: "phone", PublicKey: []byte{1}, PairedAt: t0.Add(time.Hour)}))
	must(t, s.Add(PairedDevice{ID: "a", Name: "mac", PublicKey: []byte{2}, PairedAt: t0}))
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
	if l := s.List(); len(l) != 2 || l[0].ID != "a" || l[1].ID != "b" {
		t.Fatalf("List order: %+v", l)
	}
	if d, ok := s.Find("phone"); !ok || d.ID != "b" {
		t.Fatal("Find by name failed")
	}
	if d, ok := s.Find("a"); !ok || d.Name != "mac" {
		t.Fatal("Find by id failed")
	}
	must(t, s.Add(PairedDevice{ID: "0123456789ab", Name: "tab", PublicKey: []byte{3}, PairedAt: t0}))
	if d, ok := s.Find("012345"); !ok || d.Name != "tab" {
		t.Fatal("Find by id prefix failed")
	}
	if _, ok := s.Find("01234"); ok {
		t.Fatal("Find accepted a too-short prefix")
	}
	must(t, s.Remove("0123456789ab"))
	if _, ok := s.Get("zzz"); ok {
		t.Fatal("Get unknown succeeded")
	}
	// Returned records must not alias the store.
	d, _ := s.Get("a")
	d.PublicKey[0] = 99
	if d2, _ := s.Get("a"); d2.PublicKey[0] != 2 {
		t.Fatal("Get aliases stored key")
	}

	// Touch: first touch saves, a touch within a minute does not.
	s.Touch("a", t0.Add(2*time.Hour))
	s.Touch("a", t0.Add(2*time.Hour+30*time.Second))
	re, err := OpenDeviceStore(path)
	must(t, err)
	if got, _ := re.Get("a"); !got.LastSeen.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("persisted LastSeen = %v", got.LastSeen)
	}
	if got, _ := s.Get("a"); !got.LastSeen.Equal(t0.Add(2*time.Hour + 30*time.Second)) {
		t.Fatalf("in-memory LastSeen = %v", got.LastSeen)
	}
	s.Touch("nope", t0) // unknown id is a no-op

	must(t, s.Remove("a"))
	if err := s.Remove("a"); err == nil {
		t.Fatal("double remove succeeded")
	}
	re, err = OpenDeviceStore(path)
	must(t, err)
	if l := re.List(); len(l) != 1 || l[0].ID != "b" || !bytes.Equal(l[0].PublicKey, []byte{1}) {
		t.Fatalf("reloaded: %+v", l)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %d entries", len(entries))
	}
}

func TestServerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	s, err := OpenServerStore(path)
	must(t, err)
	must(t, s.Put(KnownServer{ID: "abcdef111111", Name: "zeta", Address: "10.0.0.1:7420"}))
	must(t, s.Put(KnownServer{ID: "abcdef222222", Name: "alpha"}))
	must(t, s.Put(KnownServer{ID: "999999000000", Name: "beta"}))

	if l := s.List(); len(l) != 3 || l[0].Name != "alpha" || l[2].Name != "zeta" {
		t.Fatalf("List: %+v", l)
	}
	tests := []struct {
		q, want string
	}{
		{"abcdef111111", "abcdef111111"},
		{"abcdef1", "abcdef111111"},
		{"ABCDEF2", "abcdef222222"},
		{"999999", "999999000000"},
		{"abcdef", ""}, // ambiguous
		{"99999", ""},  // too short
		{"beta", "999999000000"},
		{"nope", ""},
	}
	for _, tt := range tests {
		k, ok := s.Find(tt.q)
		if ok != (tt.want != "") || k.ID != tt.want {
			t.Errorf("Find(%q) = %q, %v; want %q", tt.q, k.ID, ok, tt.want)
		}
	}

	must(t, s.Put(KnownServer{ID: "abcdef111111", Name: "zeta", Address: "10.0.0.2:7420"}))
	must(t, s.Remove("999999000000"))
	re, err := OpenServerStore(path)
	must(t, err)
	if l := re.List(); len(l) != 2 {
		t.Fatalf("reloaded: %+v", l)
	}
	if k, _ := re.Find("zeta"); k.Address != "10.0.0.2:7420" {
		t.Fatalf("Put did not replace: %+v", k)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
}

func TestOpenStoreCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	os.WriteFile(path, []byte("{"), 0o600)
	if _, err := OpenDeviceStore(path); err == nil {
		t.Fatal("expected error")
	}
	if _, err := OpenServerStore(path); err == nil {
		t.Fatal("expected error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
