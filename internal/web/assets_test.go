package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAssets checks that static files carry an ETag that answers 304,
// come gzipped to a client that takes it, and that sw.js names the
// version of the files.
func TestAssets(t *testing.T) {
	ts := startServer(t, newFakeSource())
	// Go's transport asks for gzip and unpacks it only when not told to.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	get := func(path string, hdr ...string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, plain := get("/app.js")
	tag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || tag == "" || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("plain app.js: %d, ETag %q, encoding %q", resp.StatusCode, tag, resp.Header.Get("Content-Encoding"))
	}
	if resp.Header.Get("Cache-Control") != "no-cache" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("headers: %v", resp.Header)
	}
	if resp, _ := get("/app.js", "If-None-Match", tag); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", resp.StatusCode)
	}

	resp, packed := get("/app.js", "Accept-Encoding", "gzip, deflate")
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("ETag") == tag || len(packed) >= len(plain) {
		t.Fatalf("gzip app.js: encoding %q, ETag %q, %d of %d bytes", resp.Header.Get("Content-Encoding"), resp.Header.Get("ETag"), len(packed), len(plain))
	}
	zr, err := gzip.NewReader(strings.NewReader(string(packed)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != string(plain) {
		t.Fatal("gzipped app.js differs from plain")
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}

	if resp, body := get("/", "Accept-Encoding", "gzip;q=0"); resp.Header.Get("Content-Encoding") != "" || !strings.Contains(string(body), "<!doctype html>") {
		t.Fatalf("index without gzip: encoding %q", resp.Header.Get("Content-Encoding"))
	}

	_, sw := get("/sw.js")
	if strings.Contains(string(sw), swVersion) || !strings.Contains(string(sw), "'"+shellVersion+"'") || len(shellVersion) != 16 {
		t.Fatalf("sw.js does not name version %q", shellVersion)
	}

	if resp, _ := get("/nope.js"); resp.StatusCode != 404 {
		t.Fatalf("missing file: %d, want 404", resp.StatusCode)
	}
}
