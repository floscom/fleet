package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"fleet/internal/skills"
)

// skillFile is the file that makes a folder a skill.
const skillFile = "SKILL.md"

// readArchive reads a skill archive (skills.Store.Archive's format: a
// gzipped tar of plain files) into memory, by path, within the store's
// limits.
func readArchive(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	var size int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s: not a plain file", hdr.Name)
		}
		if size += hdr.Size; size > skills.MaxSize || len(out) >= skills.MaxFiles {
			return nil, errors.New("the skill is too large")
		}
		b, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return nil, err
		}
		out[hdr.Name] = b
	}
}

var (
	// fenceRE opens or closes YAML front matter.
	fenceRE = regexp.MustCompile(`^(---|\.\.\.)[ \t]*\r?$`)
	// keyRE is a top-level key of front matter, and the rest of its line.
	keyRE = regexp.MustCompile(`^["']?([A-Za-z_][A-Za-z0-9_-]*)["']?[ \t]*:[ \t]*(.*?)\r?$`)
)

// frontMatter returns the lines of SKILL.md's YAML front matter, without
// the fences; nil if it has none.
func frontMatter(text string) []string {
	lines := strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "---") || !fenceRE.MatchString(lines[0]) {
		return nil
	}
	for i := 1; i < len(lines); i++ {
		if fenceRE.MatchString(lines[i]) {
			return lines[1:i]
		}
	}
	return nil
}

// maxDescription bounds a description as shown.
const maxDescription = 1024

// describe returns the description in SKILL.md's front matter: a plain or
// quoted scalar, or a block (| or >) or plain value continued on indented
// lines, folded into one line. It is a reading for display, not a YAML
// parser.
func describe(skillMD []byte) string {
	fm := frontMatter(string(skillMD))
	for i, l := range fm {
		m := keyRE.FindStringSubmatch(l)
		if m == nil || m[1] != "description" {
			continue
		}
		parts := []string{}
		v := strings.TrimSpace(m[2])
		if v != "" && !strings.HasPrefix(v, "|") && !strings.HasPrefix(v, ">") {
			parts = append(parts, v)
		}
		for _, c := range fm[i+1:] {
			if c = strings.TrimRight(c, "\r"); strings.TrimSpace(c) == "" {
				continue
			}
			if c[0] != ' ' && c[0] != '\t' {
				break
			}
			parts = append(parts, strings.TrimSpace(c))
		}
		d := strings.Join(parts, " ")
		if len(d) >= 2 && (d[0] == '"' && d[len(d)-1] == '"' || d[0] == '\'' && d[len(d)-1] == '\'') {
			d = d[1 : len(d)-1]
		}
		if len(d) > maxDescription {
			d = strings.ToValidUTF8(d[:maxDescription], "") + "…"
		}
		return d
	}
	return ""
}

// renamed is SKILL.md text as the store installs it under another name:
// the name line of its front matter replaced (skills.Store.InstallTeam
// does the same), so a diff against the installed files does not show the
// rename.
func renamed(skillMD []byte, name string) []byte {
	text := string(skillMD)
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || !fenceRE.MatchString(strings.TrimRight(strings.TrimPrefix(lines[0], "\ufeff"), "\n")) {
		return skillMD
	}
	for i := 1; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\n")
		if fenceRE.MatchString(l) {
			break
		}
		if m := keyRE.FindStringSubmatch(l); m != nil && m[1] == "name" {
			eol := lines[i][len(l):]
			if strings.HasSuffix(l, "\r") {
				eol = "\r" + eol
			}
			lines[i] = `name: "` + name + `"` + eol
			return []byte(strings.Join(lines, ""))
		}
	}
	return skillMD
}
