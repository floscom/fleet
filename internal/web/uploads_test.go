package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleet/internal/discovery"
)

type uploaded struct {
	Path string
	Size int64
}

func TestUpload(t *testing.T) {
	dir := t.TempDir()
	up := filepath.Join(dir, "uploads")
	src := newFakeSource()
	src.uploadDir = up
	tokPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokPath)
	ts := startServerToken(t, src, tokPath)

	// Old uploads go when a new one comes.
	os.MkdirAll(up, 0o700)
	old := filepath.Join(up, "1-old.mp4")
	os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-uploadTTL - time.Hour)
	os.Chtimes(old, past, past)

	var u uploaded
	if code := api(t, ts, "POST", "/api/uploads?name="+"My%20Clip%20(1).mov", tok, "movie bytes", &u); code != 201 || u.Size != 11 {
		t.Fatalf("upload: %d %+v", code, u)
	}
	if filepath.Dir(u.Path) != up || !strings.HasSuffix(u.Path, "-My-Clip--1-.mov") {
		t.Fatalf("path %q", u.Path)
	}
	if b, err := os.ReadFile(u.Path); err != nil || string(b) != "movie bytes" {
		t.Fatalf("saved %q, %v", b, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old upload kept: %v", err)
	}

	if code := api(t, ts, "POST", "/api/uploads?name=a.mp4", "", "x", nil); code != 401 {
		t.Fatalf("without token: %d", code)
	}
	if code := api(t, ts, "POST", "/api/uploads?name=a.mp4", tok, "", nil); code != 400 {
		t.Fatalf("empty: %d", code)
	}
	if entries, _ := os.ReadDir(up); len(entries) != 1 {
		t.Fatalf("upload dir holds %d files, want the one upload", len(entries))
	}
	src.uploadDir = ""
	if code := api(t, ts, "POST", "/api/uploads?name=a.mp4", tok, "x", nil); code != 404 {
		t.Fatalf("no upload dir: %d", code)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"clip.mp4":                        "clip.mp4",
		"../../etc/passwd":                "passwd",
		`C:\Users\me\Screen Rec.mov`:      "Screen-Rec.mov",
		".hidden":                         "hidden",
		"":                                "upload",
		"..":                              "upload",
		"vidéo.webm":                      "vid-o.webm",
		strings.Repeat("a", 200) + ".mkv": strings.Repeat("a", maxUploadName-4) + ".mkv",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// signedUpload posts body to ts's /api/uploads signed for target with sum
// as the announced hash.
func signedUpload(t *testing.T, ts *httptest.Server, key, target, body string, sum []byte) int {
	t.Helper()
	nonce := getNonce(t, ts)
	req, _ := http.NewRequest("POST", ts.URL+"/api/uploads?name=v.mp4", strings.NewReader(body))
	mac := peerMACSum(key, target, nonce, "POST", req.URL.RequestURI(), sum)
	req.Header.Set("Authorization", peerScheme+nonce+"."+hex.EncodeToString(mac))
	if sum != nil {
		req.Header.Set(bodyHashHeader, hex.EncodeToString(sum))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestSignedUpload(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	key, _ := LoadOrCreateToken(keyPath)
	src := newFakeSource()
	src.id = "b-id"
	src.uploadDir = filepath.Join(dir, "uploads")
	ts := startServerOpts(t, Options{Source: src, KeyPath: keyPath})

	sum := sha256.Sum256([]byte("video"))
	if code := signedUpload(t, ts, key, "b-id", "video", sum[:]); code != 201 {
		t.Fatalf("signed upload: %d", code)
	}
	// Signed for another body: refused once read, and nothing is kept.
	if code := signedUpload(t, ts, key, "b-id", "tampered", sum[:]); code != 401 {
		t.Fatalf("tampered upload: %d", code)
	}
	if code := signedUpload(t, ts, key, "b-id", "video", nil); code != 401 {
		t.Fatalf("upload without its hash: %d", code)
	}
	if entries, _ := os.ReadDir(src.uploadDir); len(entries) != 1 {
		t.Fatalf("upload dir holds %d files, want 1", len(entries))
	}
}

func TestForwardedUpload(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	LoadOrCreateToken(keyPath)
	bSrc := newFakeSource()
	bSrc.id = "b-id"
	bSrc.uploadDir = filepath.Join(dir, "b-uploads")
	b := startServerOpts(t, Options{Source: bSrc, KeyPath: keyPath})
	h, port := hostPort(t, b)
	found := []discovery.Found{{ServerID: "b-id", Name: "b", Addrs: []string{h}, WebPort: port}}
	tokPath := filepath.Join(dir, "web-token")
	tok, _ := LoadOrCreateToken(tokPath)
	aSrc := newFakeSource()
	aSrc.uploadDir = filepath.Join(dir, "a-uploads")
	a := startServerOpts(t, Options{Source: aSrc, TokenPath: tokPath, KeyPath: keyPath,
		Browse: func(context.Context, time.Duration) ([]discovery.Found, error) { return found, nil }})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(a), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {a.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	for {
		var m msg
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "peers" && m.Complete {
			break
		}
	}

	body := strings.Repeat("frame", 100000)
	req, _ := http.NewRequest("POST", a.URL+"/api/hosts/b-id/uploads?name=screen.webm", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var u uploaded
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || resp.StatusCode != 201 {
		t.Fatalf("forwarded upload: %d %+v %v", resp.StatusCode, u, err)
	}
	if filepath.Dir(u.Path) != bSrc.uploadDir || !strings.HasSuffix(u.Path, "-screen.webm") {
		t.Fatalf("path %q", u.Path)
	}
	if got, err := os.ReadFile(u.Path); err != nil || string(got) != body {
		t.Fatalf("saved %d bytes, %v", len(got), err)
	}
	// The spooled copy on a is gone.
	if entries, _ := os.ReadDir(aSrc.uploadDir); len(entries) != 0 {
		t.Fatalf("a kept %d files", len(entries))
	}
}
