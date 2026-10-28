package gemini

import (
	"strings"

	"fleet/internal/adapter"
)

// What Gemini 0.66.0 draws at the bottom of its screen while it waits on
// the user, each at the start of a line inside the dialog's box.

// screenLines is how many non-blank lines at the bottom of the screen a
// dialog or a sign-in step is looked for in.
const screenLines = 14

// trustQuestion is the folder trust dialog's question.
const trustQuestion = "Do you trust the files in this folder?"

// dialogMarks start lines only dialogs show: tool approvals ("1. Allow
// once", the question above it), questions (their key hints), menus such
// as /model, the trust and sign-in dialogs, loop detection and the usage
// limit dialog. The prompt box and its footer are gone meanwhile.
var dialogMarks = []string{
	"Allow once", "Apply this change?", "Allow execution of", "Do you want to proceed?",
	"Ready to start implementation?", "Enter to select", "(Press Esc to close)", trustQuestion,
	"How would you like to authenticate", "A potential loop was detected", "How would you like to proceed?",
}

// DetectPrompt recognizes the folder trust dialog.
func (g *gemini) DetectPrompt(screen string) (string, bool) {
	if adapter.LineStarting(adapter.BottomLines(screen, screenLines), trustQuestion) >= 0 {
		return "Gemini asks whether to trust this folder; answer in its terminal", true
	}
	return "", false
}

// DialogOpen implements adapter.DialogDetector.
func (g *gemini) DialogOpen(screen string) bool {
	bottom := adapter.BottomLines(screen, screenLines)
	for _, m := range dialogMarks {
		if adapter.LineStarting(bottom, m) >= 0 {
			return true
		}
	}
	return false
}

// Gemini signs in from its terminal (0.66.0): /auth, or a first start,
// asks how ("Sign in with Google", "Use Gemini API Key", "Vertex AI").
// Signing in with Google restarts Gemini, which then prints the URL of
// Google's sign-in page and "Enter the authorization code:". The page
// (codeassist.google.com) shows the code, so a browser on any device
// completes it. Never send an empty code: Gemini starts over with a new
// URL, and exits after the second failure. Where the agent's machine has a
// display, Gemini offers to open a browser there instead, which hands the
// sign-in to a server on that machine.

// DetectLogin implements adapter.Loginer.
func (g *gemini) DetectLogin(screen string) (adapter.Login, bool) {
	bottom := adapter.BottomLines(screen, screenLines)
	has := func(s string) bool { return adapter.LineStarting(bottom, s) >= 0 }
	switch {
	case has("Enter the authorization code"):
		return adapter.Login{
			Detail: "Sign in to Gemini: open the page, then paste the code it shows",
			URL:    adapter.ScreenURL(strings.Split(screen, "\n"), "https://"),
			Paste:  true,
		}, true
	case has("Enter Gemini API Key"), has("Paste your API key here"):
		return adapter.Login{Detail: "Gemini asks for a Gemini API key", Paste: true}, true
	case has("Opening authentication page in your browser"):
		return adapter.Login{Detail: "Gemini offers to open the sign-in page in a browser on its machine", Local: true}, true
	case has("How would you like to authenticate"):
		return adapter.Login{Detail: "Gemini asks how to sign in"}, true
	}
	return adapter.Login{}, false
}
