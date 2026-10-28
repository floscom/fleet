package codex

import (
	"testing"

	"fleet/internal/adapter"
)

// Codex 0.160.1's sign-in steps at its first start, as tmux captures them
// at 120 columns.
const (
	loginMethodScreen = `  Welcome to Codex, OpenAI's command-line coding agent

  Sign in with ChatGPT to use Codex as part of your paid plan
  or connect an API key for usage-based billing

> 1. Sign in with ChatGPT
     Usage included with Plus, Pro, Business, and Enterprise plans

  2. Sign in with Device Code
     Sign in from another device with a one-time code

  3. Provide your own API key
     Pay for what you use

  Press enter to continue
`
	deviceScreen = `  Welcome to Codex, OpenAI's command-line coding agent

  Finish signing in via your browser

  1. Open this link in your browser and sign in

  https://auth.openai.com/codex/device

  2. Enter this one-time code after you are signed in (expires in 15 minutes)

  L4U0-4E0V3

  Continue only if you started this login in Codex. If a website or another person gave you this code, cancel.

  Press esc to cancel
`
	browserScreen = `  Welcome to Codex, OpenAI's command-line coding agent

  Finish signing in via your browser

  If the link doesn't open automatically, press c to copy it:

  https://auth.openai.com/oauth/authorize?response_type=code&client_id=app_EMoamEEZ73f0CkXaXp7hrann&redirect_uri=http%3A
%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback&code_challenge=ZNjlbIuN5PTeZfYzYor3HqF9kklFP_VlsUDaJ94meaw&code_challenge_metho
d=S256&state=rkW71e9QMnvrAC4kOwuIxxl-3yj-4mChdS34PfDKOfY&scope=openid+profile+email+offline_access+api.connectors.read+a
pi.connectors.invoke&id_token_add_organizations=true&codex_cli_simplified_flow=true&originator=codex-tui

  On a remote or headless machine? Press esc and choose Sign in with Device Code.

  Press esc to cancel
`
	browserURL = "https://auth.openai.com/oauth/authorize?response_type=code&client_id=app_EMoamEEZ73f0CkXaXp7hrann&redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback&code_challenge=ZNjlbIuN5PTeZfYzYor3HqF9kklFP_VlsUDaJ94meaw&code_challenge_method=S256&state=rkW71e9QMnvrAC4kOwuIxxl-3yj-4mChdS34PfDKOfY&scope=openid+profile+email+offline_access+api.connectors.read+api.connectors.invoke&id_token_add_organizations=true&codex_cli_simplified_flow=true&originator=codex-tui"
)

func TestDetectLogin(t *testing.T) {
	c := New("", nil).(adapter.Loginer)
	if l, ok := c.DetectLogin(loginMethodScreen); !ok || l.URL != "" || l.Detail == "" {
		t.Errorf("login method: %+v, %v", l, ok)
	}
	if l, ok := c.DetectLogin(deviceScreen); !ok || l.URL != "https://auth.openai.com/codex/device" || l.Code != "L4U0-4E0V3" || l.Local || l.Paste {
		t.Errorf("device code: %+v, %v", l, ok)
	}
	if l, ok := c.DetectLogin(browserScreen); !ok || l.URL != browserURL || !l.Local {
		t.Errorf("browser: %+v, %v", l, ok)
	}
	for _, screen := range []string{
		"› Ask Codex to do anything\n\n  100% context left",
		// The trust dialog also ends in "Press enter to continue".
		"  Do you trust the contents of this directory?\n\n› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue",
	} {
		if l, ok := c.DetectLogin(screen); ok {
			t.Errorf("detected %+v in\n%s", l, screen)
		}
	}
}
