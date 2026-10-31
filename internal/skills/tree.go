package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// MaxSize caps the files of one skill, in bytes: a bigger skill stays
	// on its machine. Its archive must fit in a reply between daemons.
	MaxSize = 16 << 20
	// MaxFiles caps the files of one skill.
	MaxFiles = 2000
)

// skillFile is the file every skill has.
const skillFile = "SKILL.md"

var (
	errTooBig  = fmt.Errorf("too large to share (at most %d MiB, %d files)", MaxSize>>20, MaxFiles)
	errNoSkill = errors.New("no " + skillFile + " yet")
)

// nameRE is what a shared skill may be called: a plain name, so backups
// ("git-commit.bak.20260722"), hidden folders and fleet's own temporary
// folders (".fleet-...") are left alone.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidName reports whether name may be the name of a shared skill.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// junkDirs and junkFiles are left out of a skill wherever they are.
var (
	junkDirs  = []string{".git", ".hg", ".svn", "node_modules", "__pycache__", ".venv", ".pytest_cache", ".mypy_cache"}
	junkFiles = []string{".DS_Store", "Thumbs.db", "desktop.ini"}
)

// junk reports whether a file or folder called name is left out: VCS
// folders, dependencies, caches, OS and editor litter.
func junk(name string, dir bool) bool {
	if dir {
		return slices.Contains(junkDirs, name)
	}
	return slices.Contains(junkFiles, name) ||
		strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".swo") ||
		strings.HasSuffix(name, ".pyc") || strings.HasPrefix(name, ".#") ||
		strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#")
}

// file is one file of a skill.
type file struct {
	rel   string // slash-separated, relative to the skill folder
	abs   string
	size  int64
	exec  bool
	mtime time.Time
}

// walk lists the files of the skill folder root (a link to a folder is
// followed), sorted by path: regular files only, no junk, no links.
func walk(root string) ([]file, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var out []file
	var size int64
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == real {
			return nil
		}
		if d.IsDir() {
			if junk(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || junk(d.Name(), false) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(real, p)
		if err != nil {
			return err
		}
		size += fi.Size()
		if size > MaxSize || len(out) >= MaxFiles {
			return errTooBig
		}
		out = append(out, file{rel: filepath.ToSlash(rel), abs: p, size: fi.Size(), exec: fi.Mode()&0o111 != 0, mtime: fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b file) int { return strings.Compare(a.rel, b.rel) })
	if !slices.ContainsFunc(out, func(f file) bool { return f.rel == skillFile }) {
		return nil, errNoSkill
	}
	return out, nil
}

// fingerprint sums what stat says about files, to notice changes without
// reading them.
func fingerprint(files []file) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%t\n", f.rel, f.size, f.mtime.UnixNano(), f.exec)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashLine is one file's line in a skill's content hash.
func hashLine(h io.Writer, rel string, exec bool, sum []byte) {
	x := "-"
	if exec {
		x = "x"
	}
	fmt.Fprintf(h, "%s\x00%s\x00%x\n", rel, x, sum)
}

// hashFiles is the content hash of a skill: its files' paths, whether they
// are executable, and their contents. Nothing else (times, other mode
// bits) is shared, so the same skill hashes alike on every machine.
func hashFiles(files []file) (string, int64, error) {
	h := sha256.New()
	var size int64
	for _, f := range files {
		sum, n, err := hashFile(f.abs)
		if err != nil {
			return "", 0, err
		}
		hashLine(h, f.rel, f.exec, sum)
		size += n
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// pack archives the skill folder root as a gzipped tar, and returns its
// content hash: that of the bytes archived, which may be newer than any
// hash taken before.
func pack(root string) ([]byte, string, error) {
	files, err := walk(root)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f.abs)
		if err != nil {
			return nil, "", err
		}
		mode := int64(0o644)
		if f.exec {
			mode = 0o755
		}
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.rel, Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256(data)
		hashLine(h, f.rel, f.exec, sum[:])
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), hex.EncodeToString(h.Sum(nil)), nil
}

// unpack extracts an archive made by pack into the empty folder dir and
// returns its content hash. Anything but plain files and folders inside
// dir is refused.
func unpack(data []byte, dir string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	var files []file
	var size int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("%s: not a plain file", hdr.Name)
		}
		rel := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(rel) || hdr.Name != filepath.ToSlash(filepath.Clean(rel)) {
			return "", fmt.Errorf("%s: not a path inside the skill", hdr.Name)
		}
		if size += hdr.Size; size > MaxSize || len(files) >= MaxFiles {
			return "", errTooBig
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		exec := hdr.Mode&0o111 != 0
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(f, io.LimitReader(tr, hdr.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		files = append(files, file{rel: hdr.Name, abs: p, exec: exec})
	}
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.rel, b.rel) })
	hash, _, err := hashFiles(files)
	return hash, err
}

// copyTree copies the skill folder src to dst (which must not exist) the
// way it would be shared: no junk, no links.
func copyTree(src, dst string) error {
	data, _, err := pack(src)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	if _, err := unpack(data, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return nil
}
