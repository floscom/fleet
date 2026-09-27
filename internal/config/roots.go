package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AddRoot validates r.Path (must exist, be a dir; stored symlink-resolved and
// absolute), picks a unique name when r.Name=="" (base name, then base-2, ...),
// rejects duplicates of the same resolved path, appends it and returns it.
// It does not Save.
func (c *Config) AddRoot(r Root) (Root, error) {
	real, err := realDir(r.Path)
	if err != nil {
		return Root{}, err
	}
	if o, dup := c.rootAt(real); dup {
		return Root{}, fmt.Errorf("%s is already root %q", real, o.Name)
	}
	if r.Name == "" {
		r.Name = c.uniqueName(filepath.Base(real))
	} else if strings.ContainsAny(r.Name, `/\`) {
		return Root{}, fmt.Errorf("invalid root name %q", r.Name)
	} else if _, dup := c.RootByName(r.Name); dup {
		return Root{}, fmt.Errorf("root %q already exists", r.Name)
	}
	r.Path = real
	c.Roots = append(c.Roots, r)
	return r, nil
}

func (c *Config) uniqueName(base string) string {
	if base == "" || base == string(filepath.Separator) || base == "." {
		base = "root"
	}
	name := base
	for i := 2; ; i++ {
		if _, dup := c.RootByName(name); !dup {
			return name
		}
		name = base + "-" + strconv.Itoa(i)
	}
}

// RemoveRoot removes by name. It does not Save.
func (c *Config) RemoveRoot(name string) error {
	for i, r := range c.Roots {
		if r.Name == name {
			c.Roots = append(c.Roots[:i:i], c.Roots[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w %q", ErrUnknownRoot, name)
}

// RootByName finds a root.
func (c *Config) RootByName(name string) (Root, bool) {
	for _, r := range c.Roots {
		if r.Name == name {
			return r, true
		}
	}
	return Root{}, false
}

// RootByPath finds the root that is the directory at path (compared
// symlink-resolved, as AddRoot does). A folder inside a root does not match.
func (c *Config) RootByPath(path string) (Root, bool) {
	real, err := realDir(path)
	if err != nil {
		return Root{}, false
	}
	return c.rootAt(real)
}

// rootAt finds the root whose path resolves to real.
func (c *Config) rootAt(real string) (Root, bool) {
	for _, o := range c.Roots {
		rr, err := filepath.EvalSymlinks(o.Path)
		if o.Path == real || (err == nil && rr == real) {
			return o, true
		}
	}
	return Root{}, false
}

// Resolve maps (root, rel) or an absolute path to a validated real directory.
// If rootName != "" the path is rel inside that root; otherwise abs must be
// absolute. The result is fully symlink-resolved (filepath.EvalSymlinks),
// must exist, must be a directory, and must lie inside the (resolved) root —
// "../" and symlink escapes are rejected with ErrOutsideRoots. Returns the
// matching root and the resolved path.
func (c *Config) Resolve(rootName, rel, abs string) (Root, string, error) {
	if rootName != "" {
		root, ok := c.RootByName(rootName)
		if !ok {
			return Root{}, "", fmt.Errorf("%w %q", ErrUnknownRoot, rootName)
		}
		if filepath.IsAbs(rel) || hasDotDot(rel) {
			return Root{}, "", fmt.Errorf("%w: %q", ErrOutsideRoots, rel)
		}
		rootReal, err := realDir(root.Path)
		if err != nil {
			return Root{}, "", fmt.Errorf("root %q: %w", root.Name, err)
		}
		target, err := realDir(filepath.Join(rootReal, rel))
		if err != nil {
			return Root{}, "", err
		}
		if !within(rootReal, target) {
			return Root{}, "", fmt.Errorf("%w: %s", ErrOutsideRoots, target)
		}
		return root, target, nil
	}

	if !filepath.IsAbs(abs) {
		return Root{}, "", fmt.Errorf("path %q is not absolute", abs)
	}
	target, err := realDir(abs)
	if err != nil {
		return Root{}, "", err
	}
	// Pick the most specific root containing target.
	var best Root
	bestLen := -1
	for _, r := range c.Roots {
		rr, err := filepath.EvalSymlinks(r.Path)
		if err != nil {
			continue
		}
		if within(rr, target) && len(rr) > bestLen {
			best, bestLen = r, len(rr)
		}
	}
	if bestLen < 0 {
		return Root{}, "", fmt.Errorf("%w: %s", ErrOutsideRoots, target)
	}
	return best, target, nil
}

// realDir returns the absolute, symlink-resolved form of path, which must be
// an existing directory.
func realDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	a, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(a)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", real)
	}
	return real, nil
}

// hasDotDot reports whether any element of p is "..".
func hasDotDot(p string) bool {
	for _, e := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if e == ".." {
			return true
		}
	}
	return false
}

// within reports whether target equals root or lies below it. Both must be
// clean absolute paths; the separator check keeps /a/code2 out of /a/code.
func within(root, target string) bool {
	if target == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(target, root)
}
