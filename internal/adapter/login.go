package adapter

import "strings"

// BottomLines returns the last n lines of screen that are not blank (or
// only the sides of a box), top first, with trailing spaces removed. A CLI
// draws its sign-in steps and dialogs at the bottom of the screen; text
// higher up may be conversation.
func BottomLines(screen string, n int) []string {
	lines := strings.Split(screen, "\n")
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if l := strings.TrimRight(lines[i], " "); strings.Trim(l, " │") != "" {
			out = append(out, l)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ScreenURL returns the last address on lines that starts with prefix
// (such as "https://"), with the lines it wraps onto joined back. A TUI
// breaks a long URL at the terminal's width into lines of its own; those
// carry no spaces and start at the left edge.
func ScreenURL(lines []string, prefix string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		at := strings.Index(l, prefix)
		if at < 0 {
			continue
		}
		u, _, _ := strings.Cut(strings.TrimSpace(l[at:]), " ")
		for _, next := range lines[i+1:] {
			next = strings.TrimRight(next, " ")
			if next == "" || next != strings.TrimLeft(next, " ") || strings.Contains(next, " ") {
				break
			}
			u += next
		}
		return u
	}
	return ""
}

// LineStarting returns the index of the first of lines that starts with s,
// past indentation, box borders and the marks of a list or menu ("2. ",
// "❯ ", "● ", "✓ "), -1
// if none does. Unlike a substring, it does not match conversation or code
// that mentions s.
func LineStarting(lines []string, s string) int {
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " \t0123456789.>❯›●○✓│"), s) {
			return i
		}
	}
	return -1
}
