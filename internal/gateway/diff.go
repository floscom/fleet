package gateway

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// maxDiffFile is the largest file shown line by line; larger ones and
	// binary ones are only said to differ.
	maxDiffFile = 256 << 10
	// maxDiffText caps a whole diff.
	maxDiffText = 1 << 20
	// maxEdits bounds the line diff of one file; past it the file is shown
	// as replaced.
	maxEdits = 2000
	// diffContext is the unchanged lines shown around a change.
	diffContext = 3
)

// DiffView is what a pending update changes in an installed team skill.
type DiffView struct {
	Skill string `json:"skill"`
	// FromMs is the version installed, ToMs the pending one.
	FromMs int64      `json:"fromMs"`
	ToMs   int64      `json:"toMs"`
	Files  []FileDiff `json:"files"`
	// Text is a unified diff of the text files.
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// FileDiff is one file of a DiffView.
type FileDiff struct {
	Path string `json:"path"`
	// Status is "added", "removed" or "changed".
	Status string `json:"status"`
	// Binary is set for a file not shown line by line (binary or large).
	Binary bool `json:"binary,omitempty"`
}

// Diff shows what the update a pin holds back for a team skill (bundle id,
// "<owner>/<name>" or installed name) in team ref would change.
func (g *Gateway) Diff(ref, skill string) (DiffView, error) {
	t, err := g.find(ref)
	if err != nil {
		return DiffView{}, err
	}
	t.op.Lock()
	defer t.op.Unlock()
	if t.st.Roster == nil {
		return DiffView{}, errorf(Conflict, "not a member yet")
	}
	id, b, err := t.bundleRef(skill)
	if err != nil {
		return DiffView{}, err
	}
	inst, _ := t.installed(id)
	if b.Pending == nil || inst == nil {
		return DiffView{}, errorf(Conflict, "no update waits for %s", skill)
	}
	data, err := os.ReadFile(t.pendingPath(id))
	if err != nil {
		return DiffView{}, errorf(Conflict, "the pending update is gone; it comes again with the next sync")
	}
	next, err := readArchive(data)
	if err != nil {
		return DiffView{}, err
	}
	if md, ok := next[skillFile]; ok && inst.Name != b.Pending.Meta.Name {
		next[skillFile] = renamed(md, inst.Name)
	}
	cur, err := readDir(filepath.Join(g.opts.Skills.Dir(), inst.Name))
	if err != nil {
		return DiffView{}, err
	}
	v := diffTrees(cur, next)
	v.Skill, v.FromMs, v.ToMs = inst.Name, inst.Source.BundleVersion, b.Pending.Version
	return v, nil
}

// readDir reads the plain files under dir, by slash path.
func readDir(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// diffTrees compares two file trees.
func diffTrees(a, b map[string][]byte) DiffView {
	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	v := DiffView{Files: []FileDiff{}}
	var out strings.Builder
	for _, p := range paths {
		old, inA := a[p]
		cur, inB := b[p]
		f := FileDiff{Path: p, Status: "changed"}
		switch {
		case !inA:
			f.Status = "added"
		case !inB:
			f.Status = "removed"
		case bytes.Equal(old, cur):
			continue
		}
		f.Binary = !isText(old) || !isText(cur)
		v.Files = append(v.Files, f)
		if v.Truncated {
			continue
		}
		var text string
		if f.Binary {
			text = fmt.Sprintf("Binary files %s and %s differ\n", oldName(p, inA), newName(p, inB))
		} else {
			text = unified(oldName(p, inA), newName(p, inB), lines(old), lines(cur))
		}
		if out.Len()+len(text) > maxDiffText {
			v.Truncated = true
			continue
		}
		out.WriteString(text)
	}
	v.Text = out.String()
	return v
}

func oldName(p string, ok bool) string {
	if !ok {
		return "/dev/null"
	}
	return "a/" + p
}

func newName(p string, ok bool) string {
	if !ok {
		return "/dev/null"
	}
	return "b/" + p
}

// isText reports whether b is shown line by line: small, UTF-8, no NUL.
func isText(b []byte) bool {
	return len(b) <= maxDiffFile && utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

// lines splits text into lines, each with its newline.
func lines(b []byte) []string {
	l := strings.SplitAfter(string(b), "\n")
	if l[len(l)-1] == "" {
		l = l[:len(l)-1]
	}
	return l
}

// edit is one line of an edit script: ' ' kept, '-' deleted, '+' added.
type edit struct {
	op   byte
	line string
}

// lineDiff returns the shortest edit script from a to b (Myers' greedy
// algorithm), or, past maxEdits, a's lines deleted and b's added.
func lineDiff(a, b []string) []edit {
	n, m := len(a), len(b)
	limit := min(n+m, maxEdits)
	// v[k+off] is the furthest x on diagonal k; trace[d] keeps v as it was
	// before round d, over the diagonals round d reads (-(d-1)..d-1).
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int
	for d := 0; d <= limit; d++ {
		if d == 0 {
			trace = append(trace, nil)
		} else {
			trace = append(trace, slices.Clone(v[off-(d-1):off+d]))
		}
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, d)
			}
		}
	}
	out := make([]edit, 0, n+m)
	for _, l := range a {
		out = append(out, edit{'-', l})
	}
	for _, l := range b {
		out = append(out, edit{'+', l})
	}
	return out
}

// backtrack walks the trace back from (len(a), len(b)) after d rounds.
func backtrack(trace [][]int, a, b []string, d int) []edit {
	at := func(d, k int) int { return trace[d][k+d-1] }
	x, y := len(a), len(b)
	var out []edit
	for ; d > 0; d-- {
		k := x - y
		var pk int
		if k == -d || k != d && at(d, k-1) < at(d, k+1) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(d, pk)
		py := px - pk
		for x > px && y > py {
			x, y = x-1, y-1
			out = append(out, edit{' ', a[x]})
		}
		if x == px {
			y--
			out = append(out, edit{'+', b[y]})
		} else {
			x--
			out = append(out, edit{'-', a[x]})
		}
	}
	for x > 0 {
		x--
		out = append(out, edit{' ', a[x]})
	}
	slices.Reverse(out)
	return out
}

// unified formats the diff of two files' lines as a unified diff with
// diffContext lines of context.
func unified(from, to string, a, b []string) string {
	es := lineDiff(a, b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	// Hunks: runs of changes, joined when fewer than 2*diffContext kept
	// lines lie between them.
	i := 0
	ai, bi := 0, 0 // line numbers (0-based) at es[i]
	for i < len(es) {
		if es[i].op == ' ' {
			i, ai, bi = i+1, ai+1, bi+1
			continue
		}
		start := max(0, i-diffContext)
		sa, sb2 := ai-(i-start), bi-(i-start)
		end := i
		for j, kept := i, 0; j < len(es); j++ {
			if es[j].op == ' ' {
				if kept++; kept > 2*diffContext {
					break
				}
				continue
			}
			kept, end = 0, j
		}
		stop := min(len(es), end+1+diffContext)
		var na, nb int
		var body strings.Builder
		for _, e := range es[start:stop] {
			switch e.op {
			case ' ':
				na, nb = na+1, nb+1
			case '-':
				na++
			case '+':
				nb++
			}
			body.WriteByte(e.op)
			body.WriteString(e.line)
			if !strings.HasSuffix(e.line, "\n") {
				body.WriteString("\n\\ No newline at end of file\n")
			}
		}
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(sa, na), hunkRange(sb2, nb))
		sb.WriteString(body.String())
		for _, e := range es[i:stop] {
			if e.op != '+' {
				ai++
			}
			if e.op != '-' {
				bi++
			}
		}
		i = stop
	}
	return sb.String()
}

// hunkRange is a unified diff range: 1-based start and count, the start
// being the line before when the count is 0.
func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
