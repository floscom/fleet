package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.4.0", "v0.3.0", true},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.0", true},
		{"v0.3.1", "v0.3.0", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.2.9", "v0.3.0", false},
		{"v0.4.0", "v0.3.0-2-gabc1234", false},
		{"v0.4.0-rc1", "v0.3.0", false},
		{"dev", "v0.3.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if Released("v0.3.0-dirty") || Released("87fa909") || !Released("v12.0.3") {
		t.Error("Released is wrong")
	}
}

// release serves a fake repository whose latest release is tag, with a
// fleet binary that prints says as its version.
func release(t *testing.T, tag, says string, corrupt bool) *httptest.Server {
	t.Helper()
	target := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	script := fmt.Sprintf("#!/bin/sh\necho 'fleet %s (protocol v1)'\n", says)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{{target + "/LICENSE", "MIT"}, {target + "/fleet", script}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f.body))
	}
	tw.Close()
	zw.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	if corrupt {
		sum[0] ^= 1
	}
	sums := hex.EncodeToString(sum[:]) + "  " + target + ".tar.gz\n"

	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sums))
	})
	mux.HandleFunc("GET /releases/download/"+tag+"/"+target+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestInstall(t *testing.T) {
	srv := release(t, "v0.4.0", "v0.4.0", false)
	src := Source{URL: srv.URL}
	ctx := context.Background()
	tag, err := src.Latest(ctx)
	if err != nil || tag != "v0.4.0" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := src.Install(ctx, tag, exe); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(exe)
	if err != nil || !strings.Contains(string(out), "fleet v0.4.0") {
		t.Fatalf("exe = %q, %v", out, err)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".fleet-update-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestInstallRefusesBadReleases(t *testing.T) {
	ctx := context.Background()
	for name, srv := range map[string]*httptest.Server{
		"checksum": release(t, "v0.4.0", "v0.4.0", true),
		"version":  release(t, "v0.4.0", "v0.3.0", false),
	} {
		exe := filepath.Join(t.TempDir(), "fleet")
		os.WriteFile(exe, []byte("old"), 0o755)
		if err := (Source{URL: srv.URL}).Install(ctx, "v0.4.0", exe); err == nil {
			t.Errorf("%s: installed", name)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Errorf("%s: exe replaced", name)
		}
	}
}
