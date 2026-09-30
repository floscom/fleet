// Package transcript turns the conversation files agent CLIs write into
// chat entries for the dashboard.
//
// Claude Code and Codex both append one JSON event per line (JSONL) to a
// transcript file while they run. Each adapter that knows its CLI's format
// parses it one line at a time (adapter.Transcripter); this package reads
// the file: forward from an offset for new lines, or backward from an
// offset for older ones. It only ever consumes complete lines, so a line
// the CLI is still writing is picked up by the next read.
package transcript

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Entry kinds.
const (
	// User is what the user typed.
	User = "user"
	// Assistant is the agent's reply text.
	Assistant = "assistant"
	// Tool is a tool call: Name, a one-line Text, the full input in Detail,
	// and Output when the format records the result with the call.
	Tool = "tool"
	// Result is the Output of the Tool entry with the same ID, for formats
	// that record it separately.
	Result = "result"
	// Note is anything else worth showing: a slash command, an interrupted
	// turn, a compacted conversation, an API error.
	Note = "note"
	// Task is a background task that ended (a workflow, a background
	// command or agent): Text sums it up, ID is the Tool entry that
	// started it, Name how it ended (completed, failed, killed), Output
	// what it returned, and Error marks one that failed.
	Task = "task"
)

// Entry is one item of a chat.
type Entry struct {
	Kind string `json:"kind"`
	// ID links a Result or a Task to its Tool. It is set only on Tool
	// entries whose output comes later, in a Result: a Tool with an ID and
	// no Result yet is still running.
	ID string `json:"id,omitempty"`
	// Name is the tool's name; for a Task, how it ended.
	Name string `json:"name,omitempty"`
	// Text is the message, the one-line summary of a tool call, or the note.
	Text string `json:"text,omitempty"`
	// Detail is a tool call's full input (a command, a diff).
	Detail string `json:"detail,omitempty"`
	// Output is what a tool call returned.
	Output string `json:"output,omitempty"`
	// Error marks a tool call that failed.
	Error bool `json:"error,omitempty"`
	// TimeMs is when it happened, ms since the epoch; 0 if unknown.
	TimeMs int64 `json:"ts,omitempty"`
}

// Parser turns one line of a transcript (without its newline) into entries.
// Lines that are not part of the conversation give none.
type Parser func(line []byte) []Entry

// Size limits for entry fields; Clip enforces them.
const (
	MaxText   = 32 << 10
	MaxDetail = 4 << 10
	MaxOutput = 8 << 10
)

// Clip shortens s to at most max bytes, cut on a rune boundary, and says
// how much was left out.
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… (%d more bytes)", len(s)-cut)
}

// Line limits.
const (
	// MaxLine is the longest line parsed; longer ones (mostly inline images)
	// become a note.
	MaxLine = 4 << 20
	// forwardChunk and backwardChunk bound the bytes one read looks at.
	forwardChunk  = 4 << 20
	backwardChunk = 1 << 20
)

// Page is a range of complete lines of a transcript.
type Page struct {
	Entries []Entry
	// Start is the offset of the first line read, End the offset just after
	// the last one. The next Forward read starts at End, the next Backward
	// read ends at Start.
	Start, End int64
	// More is set when Forward stopped early: more complete lines follow.
	More bool
}

// Forward reads the complete lines from offset from, which must be the
// start of a line (0, or an End returned before). A file that shrank below
// from (replaced) gives an error wrapping ErrShrunk.
func Forward(path string, from int64, parse Parser) (Page, error) {
	f, size, err := open(path)
	if err != nil {
		return Page{}, err
	}
	defer f.Close()
	if from > size {
		return Page{}, fmt.Errorf("%s: %w", path, ErrShrunk)
	}
	return read(f, from, from+forwardChunk, parse)
}

// Backward reads about the last backwardChunk bytes of complete lines that
// end at or before offset before, which must be the start of a line or the
// file size. Page.Start is 0 once the file's first line was read.
func Backward(path string, before int64, parse Parser) (Page, error) {
	f, size, err := open(path)
	if err != nil {
		return Page{}, err
	}
	defer f.Close()
	if before > size {
		return Page{}, fmt.Errorf("%s: %w", path, ErrShrunk)
	}
	// Widen the window until it holds the start of a line before `before`
	// (a single line can be longer than the chunk).
	start := before
	for window := int64(backwardChunk); start >= before && before > 0; window *= 2 {
		from := max(0, before-window)
		if start, err = lineStart(f, from, before); err != nil {
			return Page{}, err
		}
		if from == 0 {
			break
		}
	}
	p, err := read(f, start, before, parse)
	p.More = false
	return p, err
}

// ErrShrunk means the file is shorter than an offset read before.
var ErrShrunk = errors.New("transcript was replaced")

func open(path string) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	return f, fi.Size(), nil
}

// lineStart returns the first line start at or after from, or limit if no
// line starts in [from, limit).
func lineStart(f *os.File, from, limit int64) (int64, error) {
	if from == 0 {
		return 0, nil
	}
	// A line starts at from if the byte before it ends a line.
	r := bufio.NewReaderSize(io.NewSectionReader(f, from-1, limit-from+1), 64<<10)
	pos := from - 1
	for {
		b, err := r.ReadSlice('\n')
		pos += int64(len(b))
		switch {
		case err == nil:
			return min(pos, limit), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return limit, nil
		default:
			return 0, err
		}
	}
}

// read parses the complete lines that start in [from, stop). A line may
// run past stop; a last line without its newline is left for later.
func read(f *os.File, from, stop int64, parse Parser) (Page, error) {
	p := Page{Start: from, End: from}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return p, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	for p.End < stop {
		n, long, complete, err := readLine(r, &line)
		if err != nil {
			return p, err
		}
		if !complete {
			return p, nil
		}
		p.End += n
		if long {
			p.Entries = append(p.Entries, Entry{Kind: Note, Text: fmt.Sprintf("(an entry of %d KB is too large to show)", n>>10)})
			continue
		}
		if len(line) > 0 {
			p.Entries = append(p.Entries, parse(line)...)
		}
	}
	// Stopped at the chunk limit: tell the caller whether lines follow.
	if _, err := r.Peek(1); err == nil {
		p.More = true
	}
	return p, nil
}

// readLine reads one line into *line (without the newline). It reports the
// bytes consumed, whether the line exceeded MaxLine (its content is then
// dropped), and whether it was complete. An incomplete line consumes
// nothing the caller counts: reading stops there.
func readLine(r *bufio.Reader, line *[]byte) (n int64, long, complete bool, err error) {
	*line = (*line)[:0]
	for {
		b, err := r.ReadSlice('\n')
		n += int64(len(b))
		if !long {
			if len(*line)+len(b) > MaxLine+1 {
				long, *line = true, (*line)[:0]
			} else {
				*line = append(*line, b...)
			}
		}
		switch {
		case err == nil:
			if !long { // a long line's content was dropped: nothing to trim
				*line = trimNewline(*line)
			}
			return n, long, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return n, long, false, nil
		default:
			return n, long, false, err
		}
	}
}

func trimNewline(b []byte) []byte {
	b = b[:len(b)-1]
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

// OneLine returns the first non-empty line of s, trimmed.
func OneLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
