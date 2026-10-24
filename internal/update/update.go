// Package update finds newer fleet releases on GitHub and installs them
// over the running binary, the way install.sh does: the archive for this
// OS and CPU, checked against the release's checksums.txt.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the repository releases are published in.
const DefaultURL = "https://github.com/floscom/fleet"

// maxArchive bounds a downloaded archive (a release is about 10 MB).
const maxArchive = 200 << 20

// Source is where releases come from.
type Source struct {
	// URL is the repository: URL/releases/latest redirects to the newest
	// release's tag, URL/releases/download/<tag>/ holds its files.
	// Default: FLEET_UPDATE_URL, else DefaultURL.
	URL string
	// HTTP is the client to use; default: one with a 5-minute timeout.
	HTTP *http.Client
}

func (s Source) url() string {
	if s.URL != "" {
		return strings.TrimRight(s.URL, "/")
	}
	if u := os.Getenv("FLEET_UPDATE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultURL
}

func (s Source) client() *http.Client {
	c := http.Client{Timeout: 5 * time.Minute}
	if s.HTTP != nil {
		c = *s.HTTP
	}
	return &c
}

// Latest returns the tag of the newest release. It reads where
// /releases/latest redirects to rather than GitHub's API, which limits
// unauthenticated callers to 60 requests an hour.
func (s Source) Latest(ctx context.Context) (string, error) {
	c := s.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.url()+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return "", fmt.Errorf("looking for the latest release: %s", resp.Status)
	}
	tag := path.Base(loc)
	if !Released(tag) {
		return "", fmt.Errorf("latest release %q is not a version", tag)
	}
	return tag, nil
}

// Install downloads release tag for this OS and CPU, checks it and puts
// its fleet binary in place of exe. A running fleet keeps its old binary
// until it is started again.
func (s Source) Install(ctx context.Context, tag, exe string) error {
	if !Released(tag) {
		return fmt.Errorf("%q is not a release", tag)
	}
	if err := Writable(exe); err != nil {
		return err
	}
	target := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	base := s.url() + "/releases/download/" + tag + "/"
	sums, err := s.get(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return err
	}
	want := checksum(sums, target+".tar.gz")
	if want == "" {
		return fmt.Errorf("%s.tar.gz is not listed in checksums.txt of %s", target, tag)
	}
	archive, err := s.get(ctx, base+target+".tar.gz", maxArchive)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("checksum mismatch for %s.tar.gz of %s", target, tag)
	}
	bin, err := extract(archive, target+"/fleet")
	if err != nil {
		return fmt.Errorf("%s.tar.gz: %w", target, err)
	}

	// Next to exe, so the rename below cannot cross filesystems.
	f, err := os.CreateTemp(filepath.Dir(exe), ".fleet-update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	_, err = f.Write(bin)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err != nil {
		return err
	}
	// A binary that cannot even print its version must not replace one
	// that works.
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmp, "version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded fleet %s does not run: %w", tag, err)
	}
	if got := strings.Fields(string(out)); len(got) < 2 || got[1] != tag {
		return fmt.Errorf("the downloaded fleet %s says it is %q", tag, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, exe)
}

// get reads url's body, up to limit bytes.
func (s Source) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// checksum finds name's sha256 in sha256sum output.
func checksum(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// extract returns the regular file name from a .tar.gz.
func extract(archive []byte, name string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in it", name)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(h.Name) == name && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// Writable tells why exe cannot be replaced, nil if it can: its folder
// must take new files, as a rename into it needs.
func Writable(exe string) error {
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".fleet-update-*")
	if err != nil {
		return fmt.Errorf("cannot replace %s: %s is not writable", exe, dir)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

var releaseRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Released reports whether v is a release version such as v0.3.0, not a
// development build (v0.3.0-4-gabc1234-dirty, a commit hash, "dev").
func Released(v string) bool { return releaseRe.MatchString(v) }

// Newer reports whether release a is newer than release b. Anything that
// is not a release is never newer.
func Newer(a, b string) bool {
	ma, mb := releaseRe.FindStringSubmatch(a), releaseRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}
