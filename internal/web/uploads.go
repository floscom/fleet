package web

// Files for a prompt that are too big to go with it, such as videos: the
// page streams each to the machine the agent runs on, which keeps it in
// its upload dir (Source.UploadDir, also mounted into sandboxes), and puts
// the path it answers into the prompt.
//
//	POST /api/uploads?name=clip.mov   the file as the body; answers {"path", "size"}
//
// Forwarded to other fleets like the rest of /api/ (see peers.go), but
// streamed: the body is spooled to disk to sign it, and the target checks
// it against the signed hash (bodyHashHeader) as it reads.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// maxUpload caps one uploaded file.
	maxUpload = 4 << 30
	// uploadTTL is how long uploads are kept: older ones are removed when
	// the next one arrives.
	uploadTTL = 30 * 24 * time.Hour
	// uploadTimeout bounds forwarding an upload to another daemon.
	uploadTimeout = time.Hour
	// maxUploadName caps the length of the file name kept from the upload.
	maxUploadName = 80
	// bodyHashHeader carries the hex SHA-256 of a forwarded upload's body,
	// so the target can check the signature before it has the body.
	bodyHashHeader = "X-Fleet-Body-Sha256"
	// partialPrefix starts the name of a file still being received.
	partialPrefix = ".partial-"
)

// uploadPath reports whether path is the upload route.
func uploadPath(path string) bool { return path == "/api/uploads" }

// apiUpload saves the body as a new file in the upload dir.
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	dir := s.opts.Source.UploadDir()
	if dir == "" {
		writeError(w, http.StatusNotFound, "this fleet takes no uploads")
		return
	}
	if r.ContentLength > maxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large (at most "+strconv.Itoa(maxUpload>>30)+" GiB)")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.writeSourceError(w, err)
		return
	}
	s.pruneUploads(dir)
	f, err := os.CreateTemp(dir, partialPrefix+"*")
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n == 0 {
		err = errors.New("empty file")
	}
	var path string
	if err == nil {
		path, err = uploadDest(dir, r.URL.Query().Get("name"))
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "file too large (at most "+strconv.Itoa(maxUpload>>30)+" GiB)")
		case errors.Is(err, errBodyMismatch):
			writeError(w, http.StatusUnauthorized, "not signed with this fleet's key")
		default:
			writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": path, "size": n})
}

// uploadDest is a new path in dir for a file uploaded as name: the
// time, a random part and name made safe to type into a prompt.
func uploadDest(dir, name string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.FormatInt(time.Now().UnixMilli(), 10)+"-"+hex.EncodeToString(b[:])+"-"+safeName(name)), nil
}

// safeName keeps letters, digits, '.', '_' and '-' of name's last element
// (others become '-'), at most maxUploadName bytes of it, extension kept.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	var sb strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			sb.WriteRune(c)
		default:
			sb.WriteByte('-')
		}
	}
	name = strings.TrimLeft(sb.String(), ".-")
	if len(name) > maxUploadName {
		ext := filepath.Ext(name)
		if len(ext) > 12 {
			ext = ""
		}
		name = name[:maxUploadName-len(ext)] + ext
	}
	if name == "" || name == "." {
		return "upload"
	}
	return name
}

// pruneUploads removes uploads older than uploadTTL, and files whose
// upload broke off more than a day ago.
func (s *Server) pruneUploads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		ttl := uploadTTL
		if strings.HasPrefix(e.Name(), partialPrefix) {
			ttl = 24 * time.Hour
		}
		if now.Sub(fi.ModTime()) > ttl {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				s.opts.Log.Debug("web ui: removed old upload", "file", e.Name())
			}
		}
	}
}

// errBodyMismatch means a forwarded body is not the one that was signed.
var errBodyMismatch = errors.New("body does not match its signature")

// checkedBody reads a forwarded body, failing at its end with
// errBodyMismatch unless its SHA-256 is want.
type checkedBody struct {
	io.ReadCloser
	h    hash.Hash
	want []byte
}

func newCheckedBody(r io.ReadCloser, want []byte) *checkedBody {
	return &checkedBody{ReadCloser: r, h: sha256.New(), want: want}
}

func (b *checkedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.h.Write(p[:n])
	if err == io.EOF && !bytes.Equal(b.h.Sum(nil), b.want) {
		return n, errBodyMismatch
	}
	return n, err
}

// spool copies r's body to a temporary file in dir (os.TempDir if ""),
// up to limit bytes, and returns it rewound with its size and SHA-256.
// The caller closes and removes it.
func spool(w http.ResponseWriter, r *http.Request, dir string, limit int64) (*os.File, int64, []byte, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, 0, nil, err
		}
	}
	f, err := os.CreateTemp(dir, partialPrefix+"forward-*")
	if err != nil {
		return nil, 0, nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, err
	}
	return f, n, h.Sum(nil), nil
}
