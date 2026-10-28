package claude

import (
	"strings"
	"testing"

	"fleet/internal/adapter"
)

// Claude Code 2.1.280's sign-in steps, as tmux captures them at 100 columns
// (the first after /login in a session, the others at a first start).
const (
	loginURLScreen = ` ▐▛███▛█   Claude Code v2.1.280
▝▜██████▀  Opus 5.5 (1M context) · Claude Max
  ▝▝ ▝▝    /tmp/lt/w

❯ /login
  ⎿  Login interrupted

▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔
   Login

   Browser didn't open? Use the url below to sign in (c to copy)

https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&resp
onse_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3A
create_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user
%3Afile_upload+user%3Aplugins&code_challenge=glkCJOtE81r_0ggZsjBiTQnHAuM2AficGYS7z_er3Nw&code_challe
nge_method=S256&state=3ICaDJ10Jr5gLdpefIoa-Nb6VX_PRb1CahKw9ejiWw4                                 

   Hold Option while selecting to use your terminal's native copy

   Paste code here if prompted >

   Esc to cancel                                     
`
	loginURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins&code_challenge=glkCJOtE81r_0ggZsjBiTQnHAuM2AficGYS7z_er3Nw&code_challenge_method=S256&state=3ICaDJ10Jr5gLdpefIoa-Nb6VX_PRb1CahKw9ejiWw4"

	loginMethodScreen = `.......█ █   █ █..........................................

 Claude Code can be used with your Claude subscription or billed based on API usage through your Console account.

 Select login method:

 ❯ 1. Claude account with subscription · Pro, Max, Team, or Enterprise
   2. Anthropic Console account · API usage billing
   3. 3rd-party platform · Amazon Bedrock, Microsoft Foundry, or Vertex AI
`
	loginErrorScreen = `       █████████      *
.......█ █   █ █..........................................

 OAuth error: Request failed with status code 400

 Press Enter to retry.
`
	themeScreen = ` Let's get started.

 Choose the text style that looks best with your terminal
 To change this later, run /theme

   1. Auto (match terminal)
 ❯ 2. Dark mode ✔
   3. Light mode

 ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
  1  function greet() {
  2 -  console.log("Hello, World!");
  2 +  console.log("Hello, Claude!");
  3  }
 ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
  Syntax theme: Monokai Extended (ctrl+t to disable)
`
)

func TestDetectLogin(t *testing.T) {
	c := New("", nil).(adapter.Loginer)
	l, ok := c.DetectLogin(loginURLScreen)
	if !ok || l.URL != loginURL || !l.Paste || l.Code != "" || l.Local {
		t.Errorf("sign-in page: %+v, %v", l, ok)
	}
	if l, ok := c.DetectLogin(loginMethodScreen); !ok || l.URL != "" || l.Paste || l.Detail == "" {
		t.Errorf("login method: %+v, %v", l, ok)
	}
	if l, ok := c.DetectLogin(loginErrorScreen); !ok || !strings.Contains(l.Detail, "status code 400") {
		t.Errorf("error: %+v, %v", l, ok)
	}
	if l, ok := c.DetectLogin("  Logged in as me@example.com\n  Login successful. Press Enter to continue…\n"); !ok || l.Detail == "" {
		t.Errorf("signed in: %+v, %v", l, ok)
	}
	for name, screen := range map[string]string{
		"prompt": "❯ \n──────\n  ⏵⏵ auto mode on (shift+tab to cycle)",
		// This very adapter, in a diff in the conversation.
		"code":  "  25 +	case has(\"Paste code here if prompted\"):\n  26 +		return adapter.Login{\n❯ \n",
		"theme": themeScreen,
	} {
		if l, ok := c.DetectLogin(screen); ok {
			t.Errorf("%s: detected %+v", name, l)
		}
	}
}

func TestDetectPromptSetup(t *testing.T) {
	c := New("", nil).(adapter.PromptDetector)
	if d, ok := c.DetectPrompt(themeScreen); !ok || d == "" {
		t.Error("text style not detected")
	}
	if _, ok := c.DetectPrompt(loginURLScreen); ok {
		t.Error("sign-in detected as a setup step")
	}
}
