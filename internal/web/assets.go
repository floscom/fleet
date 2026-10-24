package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// asset is a static file kept in memory with its ETag, and gzipped when
// that makes it smaller.
type asset struct {
	name  string // for the Content-Type
	plain []byte
	gz    []byte // nil: not worth it
	tag   string
}

var (
	assetsOnce sync.Once
	assets     map[string]*asset // by URL path
	// shellVersion changes with any static file: the service worker keeps
	// the page's files under it and fetches them again when it changes.
	shellVersion string
)

// swVersion is the placeholder in sw.js that is served as shellVersion.
const swVersion = "__SHELL_VERSION__"

func loadAssets() {
	assets = map[string]*asset{}
	sum := sha256.New()
	var paths []string
	_ = fs.WalkDir(staticFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	for _, p := range paths {
		b, err := fs.ReadFile(staticFS, p)
		if err != nil {
			continue
		}
		sum.Write([]byte(p))
		sum.Write(b)
		url := "/" + p
		if p == "index.html" {
			url = "/"
		}
		assets[url] = &asset{name: path.Base(p), plain: b}
	}
	shellVersion = hex.EncodeToString(sum.Sum(nil))[:16]
	if sw := assets["/sw.js"]; sw != nil {
		sw.plain = bytes.ReplaceAll(sw.plain, []byte(swVersion), []byte(shellVersion))
	}
	for _, a := range assets {
		h := sha256.Sum256(a.plain)
		a.tag = `"` + hex.EncodeToString(h[:8]) + `"`
		if compressible(a.name) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(a.plain)
			zw.Close()
			if buf.Len() < len(a.plain)*9/10 {
				a.gz = buf.Bytes()
			}
		}
	}
}

func compressible(name string) bool {
	for _, ext := range []string{".html", ".js", ".css", ".svg", ".webmanifest", ".txt", ".ico"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// serveAsset serves the static file at r's path, and reports false if
// there is none (a directory, or nothing).
func serveAsset(w http.ResponseWriter, r *http.Request) bool {
	assetsOnce.Do(loadAssets)
	a := assets[r.URL.Path]
	if a == nil {
		return false
	}
	h := w.Header()
	body, tag := a.plain, a.tag
	if a.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			body, tag = a.gz, strings.TrimSuffix(a.tag, `"`)+`-gz"`
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", tag)
	http.ServeContent(w, r, a.name, time.Time{}, bytes.NewReader(body))
	return true
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(enc) == "gzip" && strings.ReplaceAll(q, " ", "") != "q=0" {
			return true
		}
	}
	return false
}
