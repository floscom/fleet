package daemon

// Session titles: what a conversation is about, for clients to list it by
// (Agent.title). Claude Code titles its sessions itself (transcript Title
// entries; the latest wins). Until it has, and for CLIs that do not, the
// title is the start of the first message. The transcript scan for pull
// requests (scanPulls) collects them on its way.

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"fleet/internal/transcript"
)

// titleMax is the most runes a title made from a message keeps.
const titleMax = 80

// titleFind collects what transcript entries say about the title: the
// CLI's latest title, and the first message that makes one.
type titleFind struct {
	cli, first string
}

func (t *titleFind) read(entries []transcript.Entry) {
	for _, e := range entries {
		switch e.Kind {
		case transcript.Title:
			if s := strings.TrimSpace(e.Text); s != "" {
				t.cli = s
			}
		case transcript.User:
			if t.first == "" {
				t.first = promptTitle(e.Text)
			}
		}
	}
}

var (
	imageRef  = regexp.MustCompile(`\[[Ii]mage(?: #\d+)?\]`)
	spaceRun  = regexp.MustCompile(`\s+`)
	sentenceE = regexp.MustCompile(`[.!?](?:\s|$)`)
)

// promptTitle makes a title of a message: its first line that says
// something, without image placeholders, cut after its first sentence or
// at a word near titleMax runes. A slash command or upload paths alone make
// none ("").
func promptTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(spaceRun.ReplaceAllString(imageRef.ReplaceAllString(line, ""), " "))
		if line == "" || strings.HasPrefix(line, "Video: ") || strings.HasPrefix(line, "PDF: ") {
			continue
		}
		if strings.HasPrefix(line, "/") {
			return ""
		}
		// The first sentence, unless it is too short to say much ("Hi.").
		if m := sentenceE.FindStringIndex(line); m != nil && utf8.RuneCountInString(line[:m[0]]) >= 12 {
			line = line[:m[0]+1]
		}
		if utf8.RuneCountInString(line) > titleMax {
			s := string([]rune(line)[:titleMax])
			if i := strings.LastIndexByte(s, ' '); i > len(s)/2 {
				s = s[:i]
			}
			return strings.TrimRight(s, " ,;:-") + "…"
		}
		return strings.TrimSuffix(line, ".")
	}
	return ""
}

// titleBackfill looks for the title of a transcript whose start was read
// before titles were: the start holds the first message (and the CLI's
// first title), the end its latest title. It returns what it found and
// how many bytes it read.
func titleBackfill(path string, parse transcript.Parser) (titleFind, int64) {
	var t titleFind
	var n int64
	if p, err := transcript.Forward(path, 0, parse); err == nil {
		t.read(p.Entries)
		n += p.End
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > n {
		if p, err := transcript.Backward(path, fi.Size(), parse); err == nil {
			var end titleFind
			end.read(p.Entries)
			if end.cli != "" {
				t.cli = end.cli
			}
			n += p.End - p.Start
		}
	}
	return t, n
}

// applyTitle gives agent a the title t says, and reports whether it
// changed: the CLI's title wins over one made from a message.
func (a *agentRec) applyTitle(t titleFind) bool {
	switch {
	case t.cli != "" && (a.Title != t.cli || a.TitleFromPrompt):
		a.Title, a.TitleFromPrompt = t.cli, false
		return true
	case t.cli == "" && a.Title == "" && t.first != "":
		a.Title, a.TitleFromPrompt = t.first, true
		return true
	}
	return false
}
