package codex

import (
	"strings"

	"fleet/internal/adapter"
)

// Codex signs in from its terminal when it starts logged out (0.160): it
// asks how ("Sign in with ChatGPT", "Sign in with Device Code", "Provide
// your own API key"). Signing in with ChatGPT shows an auth.openai.com URL,
// broken into lines at the terminal's width, whose page redirects to a
// server Codex runs on 127.0.0.1:1455, so only a browser on the agent's
// machine completes it. The device code shows a page and a one-time code to
// enter there, from any device. Then: "Signed in with your ChatGPT account".
// The steps are drawn at the bottom of the screen.

// loginLines is how many non-blank lines at the bottom of the screen a
// sign-in step is looked for in.
const loginLines = 12

// DetectLogin implements adapter.Loginer.
func (c *codex) DetectLogin(screen string) (adapter.Login, bool) {
	bottom := adapter.BottomLines(screen, loginLines)
	has := func(s string) bool { return adapter.LineStarting(bottom, s) >= 0 }
	switch {
	case has("Enter this one-time code"):
		l := adapter.Login{
			Detail: "Sign in to Codex: open the page and enter the code",
			URL:    adapter.ScreenURL(bottom, "https://"),
		}
		if i := adapter.LineStarting(bottom, "Enter this one-time code"); i+1 < len(bottom) {
			l.Code = strings.TrimSpace(bottom[i+1])
		}
		return l, true
	case has("Finish signing in via your browser"):
		u := adapter.ScreenURL(strings.Split(screen, "\n"), "https://")
		return adapter.Login{
			Detail: "Sign in to Codex in a browser on its machine, or press Esc for a device code",
			URL:    u,
			Local:  strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost"),
		}, true
	case has("Paste or type your API key"):
		return adapter.Login{Detail: "Codex asks for an OpenAI API key", Paste: true}, true
	case has("Sign in with Device Code") && has("Press enter to continue"):
		return adapter.Login{Detail: "Codex asks how to sign in: Sign in with Device Code works from any device"}, true
	case has("Signed in with your ChatGPT account"), has("API key configured"):
		return adapter.Login{Detail: "Signed in to Codex: press Enter to continue"}, true
	}
	return adapter.Login{}, false
}
