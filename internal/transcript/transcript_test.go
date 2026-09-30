package transcript

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// lineParser makes one Note per line, so tests can see exactly which lines
// were read.
func lineParser(line []byte) []Entry {
	return []Entry{{Kind: Note, Text: string(line)}}
}

func texts(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Text
	}
	return out
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestForward(t *testing.T) {
	p := writeFile(t, "a\nbb\n\nccc\n")
	pg, err := Forward(p, 0, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	// Empty lines are skipped but consumed.
	if got := texts(pg.Entries); !slices.Equal(got, []string{"a", "bb", "ccc"}) {
		t.Errorf("entries %q", got)
	}
	if pg.Start != 0 || pg.End != 10 || pg.More {
		t.Errorf("page %d-%d more %v", pg.Start, pg.End, pg.More)
	}

	// Nothing new yet.
	pg, err = Forward(p, pg.End, lineParser)
	if err != nil || len(pg.Entries) != 0 || pg.Start != 10 || pg.End != 10 {
		t.Fatalf("at end: %+v, %v", pg, err)
	}

	appendFile(t, p, "dd\ne\n")
	pg, err = Forward(p, pg.End, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); !slices.Equal(got, []string{"dd", "e"}) || pg.Start != 10 || pg.End != 15 {
		t.Errorf("after append: %q %d-%d", got, pg.Start, pg.End)
	}
}

func TestForwardPartialLine(t *testing.T) {
	p := writeFile(t, "one\ntw")
	pg, err := Forward(p, 0, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); !slices.Equal(got, []string{"one"}) || pg.End != 4 || pg.More {
		t.Fatalf("%q end %d more %v", got, pg.End, pg.More)
	}
	// The unfinished line is read once it is complete.
	pg, _ = Forward(p, pg.End, lineParser)
	if len(pg.Entries) != 0 || pg.End != 4 {
		t.Fatalf("still partial: %+v", pg)
	}
	appendFile(t, p, "o\nthr")
	pg, _ = Forward(p, pg.End, lineParser)
	if got := texts(pg.Entries); !slices.Equal(got, []string{"two"}) || pg.End != 8 {
		t.Fatalf("completed: %q end %d", got, pg.End)
	}
}

func TestCRLF(t *testing.T) {
	p := writeFile(t, "a\r\nb\r\n\r\nc\n")
	pg, err := Forward(p, 0, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("Forward %q", got)
	}
	pg, err = Backward(p, pg.End, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); !slices.Equal(got, []string{"a", "b", "c"}) || pg.Start != 0 {
		t.Errorf("Backward %q start %d", got, pg.Start)
	}
}

// numbered writes n lines "line 000000 xxx..." of about width bytes.
func numbered(n, width int) (string, []string) {
	var b strings.Builder
	lines := make([]string, n)
	for i := range lines {
		l := fmt.Sprintf("line %06d ", i)
		l += strings.Repeat("x", max(0, width-len(l)))
		lines[i] = l
		b.WriteString(l + "\n")
	}
	return b.String(), lines
}

func TestBackwardPaging(t *testing.T) {
	// About 2.5 backward chunks of lines.
	content, want := numbered(25000, 100)
	p := writeFile(t, content)
	size := int64(len(content))

	var pages [][]string
	before := size
	for {
		pg, err := Backward(p, before, lineParser)
		if err != nil {
			t.Fatal(err)
		}
		if pg.End != before || pg.More {
			t.Fatalf("page ends at %d (more %v), want %d", pg.End, pg.More, before)
		}
		if pg.Start >= before {
			t.Fatalf("no progress from %d", before)
		}
		if pg.End-pg.Start > backwardChunk {
			t.Errorf("page of %d bytes", pg.End-pg.Start)
		}
		pages = append(pages, texts(pg.Entries))
		if pg.Start == 0 {
			break
		}
		before = pg.Start
	}
	if len(pages) < 3 {
		t.Errorf("%d pages, want several", len(pages))
	}
	var got []string
	for i := len(pages) - 1; i >= 0; i-- {
		got = append(got, pages[i]...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backward pages hold %d lines, want %d in order", len(got), len(want))
	}

	// Backward from the end then Forward from its End: the two meet.
	appendFile(t, p, "new\n")
	last, _ := Backward(p, size, lineParser)
	next, err := Forward(p, last.End, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(next.Entries); !slices.Equal(got, []string{"new"}) {
		t.Errorf("forward after backward: %q", got)
	}
}

func TestForwardMore(t *testing.T) {
	// More than one forward chunk.
	content, want := numbered(50000, 100)
	p := writeFile(t, content)
	first, err := Forward(p, 0, lineParser)
	if err != nil || !first.More || first.End >= int64(len(content)) || first.End-first.Start < forwardChunk {
		t.Fatalf("first read %d-%d more %v, %v", first.Start, first.End, first.More, err)
	}
	if got := texts(forwardAll(t, p)); !slices.Equal(got, want) {
		t.Errorf("read %d lines, want %d in order", len(got), len(want))
	}
}

// forwardAll reads the whole file with Forward.
func forwardAll(t *testing.T, p string) []Entry {
	t.Helper()
	var out []Entry
	for from := int64(0); ; {
		pg, err := Forward(p, from, lineParser)
		if err != nil {
			t.Fatal(err)
		}
		out, from = append(out, pg.Entries...), pg.End
		if !pg.More {
			return out
		}
	}
}

// backwardAll reads the whole file with Backward, from before.
func backwardAll(t *testing.T, p string, before int64) []Entry {
	t.Helper()
	var out []Entry
	for {
		pg, err := Backward(p, before, lineParser)
		if err != nil {
			t.Fatal(err)
		}
		out = append(pg.Entries, out...)
		if pg.Start == 0 {
			return out
		}
		before = pg.Start
	}
}

func TestLongLine(t *testing.T) {
	long := strings.Repeat("y", MaxLine+10)
	content := "before\n" + long + "\nafter\n"
	p := writeFile(t, content)
	for name, es := range map[string][]Entry{
		"Forward":  forwardAll(t, p),
		"Backward": backwardAll(t, p, int64(len(content))),
	} {
		got := texts(es)
		if len(got) != 3 || got[0] != "before" || got[2] != "after" ||
			!strings.Contains(got[1], "too large") || es[1].Kind != Note {
			t.Errorf("%s: entries %.80q", name, got)
		}
	}

	// An unfinished long line is left for later.
	p = writeFile(t, "a\n"+long)
	pg, err := Forward(p, 0, lineParser)
	if err != nil || !slices.Equal(texts(pg.Entries), []string{"a"}) || pg.End != 2 {
		t.Errorf("partial long line: %.80q end %d, %v", texts(pg.Entries), pg.End, err)
	}
}

func TestBackwardLineLongerThanChunk(t *testing.T) {
	big := strings.Repeat("z", backwardChunk+backwardChunk/2)
	content := "first\n" + big + "\nlast\n"
	p := writeFile(t, content)

	pg, err := Backward(p, int64(len(content)), lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); !slices.Equal(got, []string{"last"}) {
		t.Fatalf("first page %.40q", got)
	}
	pg, err = Backward(p, pg.Start, lineParser)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(pg.Entries); len(got) != 2 || got[0] != "first" || got[1] != big || pg.Start != 0 {
		t.Fatalf("second page %.40q start %d", got, pg.Start)
	}

	// A file that is a single line.
	p = writeFile(t, big+"\n")
	pg, err = Backward(p, int64(len(big)+1), lineParser)
	if err != nil || len(pg.Entries) != 1 || pg.Start != 0 {
		t.Fatalf("single line: %d entries start %d, %v", len(pg.Entries), pg.Start, err)
	}
}

func TestBackwardEmpty(t *testing.T) {
	p := writeFile(t, "")
	pg, err := Backward(p, 0, lineParser)
	if err != nil || len(pg.Entries) != 0 || pg.Start != 0 || pg.End != 0 {
		t.Fatalf("%+v, %v", pg, err)
	}
}

func TestShrunk(t *testing.T) {
	p := writeFile(t, "a\nb\n")
	if _, err := Forward(p, 5, lineParser); !errors.Is(err, ErrShrunk) {
		t.Errorf("Forward past the end: %v", err)
	}
	if _, err := Backward(p, 5, lineParser); !errors.Is(err, ErrShrunk) {
		t.Errorf("Backward past the end: %v", err)
	}
	if _, err := Forward(filepath.Join(t.TempDir(), "missing"), 0, lineParser); err == nil {
		t.Error("missing file: no error")
	}
	if _, err := Forward(t.TempDir(), 0, lineParser); err == nil {
		t.Error("directory: no error")
	}
}

func TestClip(t *testing.T) {
	if got := Clip("short", 10); got != "short" {
		t.Errorf("Clip = %q", got)
	}
	got := Clip("aé€b", 4) // "a" + 2-byte é + 3-byte €: the cut falls inside €
	if !strings.HasPrefix(got, "aé\n") || !strings.Contains(got, "(4 more bytes)") || !utf8.ValidString(got) {
		t.Errorf("Clip = %q", got)
	}
}

func TestOneLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"one":              "one",
		"\n  \n  two  \nx": "two",
	} {
		if got := OneLine(in); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
}
