package claude

import (
	"strings"

	"fleet/internal/adapter"
)

// Claude Code signs in from its terminal, at first start or with /login
// (2.1.280): it asks for a login method ("Select login method:"), then shows
// the sign-in page's URL, broken into lines at the terminal's width, above
// "Paste code here if prompted >". The page (platform.claude.com) shows a
// code to paste there, so a browser on any device completes it. Then comes
// "Login successful. Press Enter to continue…", or "OAuth error: ...
// Press Enter to retry." The steps are drawn at the bottom of the screen.

// loginLines is how many non-blank lines at the bottom of the screen a
// sign-in step is looked for in.
const loginLines = 10

// DetectLogin implements adapter.Loginer.
func (c *claude) DetectLogin(screen string) (adapter.Login, bool) {
	bottom := adapter.BottomLines(screen, loginLines)
	has := func(s string) bool {
		return adapter.LineStarting(bottom, s) >= 0
	}
	switch {
	case has("Paste code here if prompted"):
		return adapter.Login{
			Detail: "Sign in to Claude: open the page, then paste the code it shows",
			URL:    adapter.ScreenURL(strings.Split(screen, "\n"), "https://"),
			Paste:  true,
		}, true
	case has("Select login method:"):
		return adapter.Login{Detail: "Claude asks how to sign in"}, true
	case has("Login successful"):
		return adapter.Login{Detail: "Signed in to Claude: press Enter to continue"}, true
	case has("Press Enter to retry"):
		if i := adapter.LineStarting(bottom, "OAuth error:"); i >= 0 {
			return adapter.Login{Detail: "Signing in failed (" + strings.TrimSpace(bottom[i]) + "): press Enter to try again"}, true
		}
	}
	return adapter.Login{}, false
}
