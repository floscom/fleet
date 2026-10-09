package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

// pullAdapter is testAdapter with tool calls in its transcripts:
//
//	tool ID COMMAND     a shell call whose output comes later
//	result ID OUTPUT    its output
//	shell COMMAND => OUTPUT   a shell call with its output
type pullAdapter struct{ testAdapter }

func (pullAdapter) ID() string { return "pulls" }

func (pullAdapter) ParseTranscript(line []byte) []transcript.Entry {
	s := string(line)
	if rest, ok := strings.CutPrefix(s, "tool "); ok {
		id, cmd, _ := strings.Cut(rest, " ")
		return []transcript.Entry{{Kind: transcript.Tool, ID: id, Name: "Bash", Text: cmd}}
	}
	if rest, ok := strings.CutPrefix(s, "result "); ok {
		id, out, _ := strings.Cut(rest, " ")
		return []transcript.Entry{{Kind: transcript.Result, ID: id, Output: out}}
	}
	if rest, ok := strings.CutPrefix(s, "shell "); ok {
		cmd, out, _ := strings.Cut(rest, " => ")
		return []transcript.Entry{{Kind: transcript.Tool, Name: "shell", Text: cmd, Output: out}}
	}
	return testAdapter{}.ParseTranscript(line)
}

// fakeGitHub is GitHub for tests: pull requests by URL.
type fakeGitHub struct {
	mu     sync.Mutex
	pulls  map[string]*fleetv1.PullRequest
	merged []string // "URL method delete"
	fail   error
}

func (f *fakeGitHub) view(_ context.Context, r prRef) (*fleetv1.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	p, ok := f.pulls[r.url]
	if !ok {
		return nil, fmt.Errorf("no pull request %s", r.url)
	}
	return &fleetv1.PullRequest{Title: p.Title, State: p.State, Draft: p.Draft, MergeState: p.MergeState, Checks: p.Checks}, nil
}

func (f *fakeGitHub) methods(context.Context, prRef) ([]fleetv1.MergeMethod, error) {
	return []fleetv1.MergeMethod{fleetv1.MergeMethod_MERGE_METHOD_SQUASH, fleetv1.MergeMethod_MERGE_METHOD_REBASE}, nil
}

func (f *fakeGitHub) ready(_ context.Context, r prRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[r.url].Draft = false
	return nil
}

func (f *fakeGitHub) merge(_ context.Context, r prRef, m fleetv1.MergeMethod, del bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pulls[r.url]
	if p.Draft {
		return fmt.Errorf("draft")
	}
	p.State = fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED
	f.merged = append(f.merged, fmt.Sprintf("%s %s %v", r.url, methodName(m), del))
	return nil
}

func TestPullRequests(t *testing.T) {
	interval := pullInterval
	pullInterval = 50 * time.Millisecond
	t.Cleanup(func() { pullInterval = interval })
	const url1, url2 = "https://github.com/acme/api/pull/7", "https://github.com/acme/api/pull/8"
	gh := &fakeGitHub{pulls: map[string]*fleetv1.PullRequest{
		url1: {Title: "Fix the tests", State: fleetv1.PullRequestState_PULL_REQUEST_STATE_OPEN, Draft: true},
		url2: {Title: "Docs", State: fleetv1.PullRequestState_PULL_REQUEST_STATE_OPEN},
	}}
	old := gitHub
	gitHub = gh
	t.Cleanup(func() { gitHub = old })

	e := newWebEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Adapter: "pulls", Root: "code", ExtraArgs: []string{"cat"}})
	id := a.GetId()
	file := filepath.Join(testAdapter{}.TranscriptDir(""), a.GetSessionId()+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(lines ...string) {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			fmt.Fprintln(f, l)
		}
		f.Close()
	}

	// Only URLs a pull request creation printed count.
	write("tool t1 gh pr view 3", "result t1 https://github.com/acme/api/pull/3",
		"tool t2 git push -u origin fix && gh pr create --fill --draft",
		"result t2 remote: Create a pull request for 'fix' on GitHub by visiting: https://github.com/acme/api/pull/new/fix "+url1)
	got := c.waitAgent(id, "the pull request looked up", func(a *fleetv1.Agent) bool {
		return len(a.GetPullRequests()) == 1 && a.GetPullRequests()[0].GetCheckedAtMs() > 0
	})
	p := got.GetPullRequests()[0]
	if p.GetUrl() != url1 || p.GetRepo() != "acme/api" || p.GetNumber() != 7 || p.GetTitle() != "Fix the tests" ||
		!p.GetDraft() || p.GetState() != fleetv1.PullRequestState_PULL_REQUEST_STATE_OPEN ||
		!slices.Equal(p.GetMergeMethods(), []fleetv1.MergeMethod{fleetv1.MergeMethod_MERGE_METHOD_SQUASH, fleetv1.MergeMethod_MERGE_METHOD_REBASE}) {
		t.Fatalf("pull request: %v", p)
	}

	// A draft is marked ready, then merged the repository's first way.
	m := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_MergePullRequest{MergePullRequest: &fleetv1.MergePullRequestRequest{Agent: a.GetName()}}}).GetMergePullRequest()
	if m.GetPullRequest().GetState() != fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED {
		t.Fatalf("merged: %v", m.GetPullRequest())
	}
	if want := url1 + " squash false"; !slices.Equal(gh.merged, []string{want}) {
		t.Fatalf("merges: %q", gh.merged)
	}
	if c.agent(id).GetPullRequests()[0].GetState() != fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED {
		t.Fatal("the agent does not show it merged")
	}
	if e := c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_MergePullRequest{MergePullRequest: &fleetv1.MergePullRequestRequest{Agent: id}}}, codeNotFound); !strings.Contains(e.GetMessage(), "no open pull request") {
		t.Fatalf("nothing left to merge: %v", e)
	}

	// A shell call with its output (Codex), merged from the dashboard.
	write("shell gh pr create --title Docs => " + url2)
	c.waitAgent(id, "the second pull request", func(a *fleetv1.Agent) bool {
		return len(a.GetPullRequests()) == 2 && a.GetPullRequests()[1].GetCheckedAtMs() > 0
	})
	var fail struct{ Error string }
	if code := e.webCall("POST", "/api/agents/"+id+"/merge", `{"pullRequest":"8","method":"merge"}`, &fail); code != 400 || !strings.Contains(fail.Error, "does not allow merge") {
		t.Fatalf("disallowed method: %d %+v", code, fail)
	}
	var r struct{ PullRequest PullRequestJSON }
	if code := e.webCall("POST", "/api/agents/"+id+"/merge", `{"pullRequest":"#8","method":"rebase","deleteBranch":true}`, &r); code != 200 || r.PullRequest.State != "merged" || r.PullRequest.Number != 8 {
		t.Fatalf("merge from the web: %d %+v", code, r)
	}
	if want := url2 + " rebase true"; gh.merged[len(gh.merged)-1] != want {
		t.Fatalf("merges: %q", gh.merged)
	}
}

// PullRequestJSON is the part of web.PullRequest the test reads.
type PullRequestJSON struct {
	Number int32
	State  string
}

func TestPRScan(t *testing.T) {
	var s prScan
	found := s.read([]transcript.Entry{
		{Kind: transcript.Tool, ID: "a", Name: "Bash", Text: `gh pr create --title "x" --body "$(cat <<'EOF'`, Detail: "gh pr create ..."},
		{Kind: transcript.Tool, ID: "b", Name: "Bash", Text: "echo https://github.com/o/r/pull/1"},
		{Kind: transcript.Result, ID: "b", Output: "https://github.com/o/r/pull/1"},
		{Kind: transcript.Tool, Name: "mcp__github__create_pull_request", Output: `{"html_url":"https://github.com/o/r/pull/5","diff_url":"https://github.com/o/r/pull/5.diff","url":"https://api.github.com/repos/o/r/pulls/5"}`},
	})
	if len(found) != 1 || found[0].ref.url != "https://github.com/o/r/pull/5" || !slices.Equal(s.Pending, []string{"a"}) {
		t.Fatalf("first: %+v %v", found, s.Pending)
	}
	found = s.read([]transcript.Entry{{Kind: transcript.Result, ID: "a", Output: "a pull request for branch \"x\" into branch \"main\" already exists:\nhttps://GitHub.com/o/r/pull/4", TimeMs: 9}})
	if len(found) != 1 || found[0].ref.url != "https://github.com/o/r/pull/4" || found[0].ref.number != 4 || found[0].timeMs != 9 || len(s.Pending) != 0 {
		t.Fatalf("result: %+v %v", found, s.Pending)
	}
	if r, ok := parsePRURL("https://ghe.example.com/team/app/pull/12/"); !ok || r.ghRepo() != "ghe.example.com/team/app" || r.number != 12 {
		t.Fatalf("enterprise: %+v %v", r, ok)
	}
	if _, ok := parsePRURL("https://github.com/o/r/pull/12/files"); ok {
		t.Fatal("parsed a sub page")
	}
}

func TestChecksState(t *testing.T) {
	for _, c := range []struct {
		checks []ghCheck
		want   fleetv1.ChecksState
	}{
		{nil, fleetv1.ChecksState_CHECKS_STATE_UNSPECIFIED},
		{[]ghCheck{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {State: "SUCCESS"}, {Status: "COMPLETED", Conclusion: "SKIPPED"}}, fleetv1.ChecksState_CHECKS_STATE_PASSING},
		{[]ghCheck{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "IN_PROGRESS"}}, fleetv1.ChecksState_CHECKS_STATE_PENDING},
		{[]ghCheck{{State: "PENDING"}}, fleetv1.ChecksState_CHECKS_STATE_PENDING},
		{[]ghCheck{{Status: "IN_PROGRESS"}, {Status: "COMPLETED", Conclusion: "FAILURE"}}, fleetv1.ChecksState_CHECKS_STATE_FAILING},
		{[]ghCheck{{State: "ERROR"}}, fleetv1.ChecksState_CHECKS_STATE_FAILING},
	} {
		if got := checksState(c.checks); got != c.want {
			t.Errorf("%+v: %v, want %v", c.checks, got, c.want)
		}
	}
}

func TestPickPull(t *testing.T) {
	open := fleetv1.PullRequestState_PULL_REQUEST_STATE_OPEN
	list := []*fleetv1.PullRequest{
		{Url: "https://github.com/o/r/pull/1", Number: 1, State: fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED},
		{Url: "https://github.com/o/r/pull/2", Number: 2, State: open},
		{Url: "https://github.com/o/r/pull/3", Number: 3},
	}
	if _, err := pickPull(list, ""); err == nil || !strings.Contains(err.Error(), "#2, #3") {
		t.Fatalf("two open: %v", err)
	}
	if p, err := pickPull(list[:2], ""); err != nil || p.Number != 2 {
		t.Fatalf("only open: %v %v", p, err)
	}
	if p, err := pickPull(list, "https://github.com/o/r/pull/3"); err != nil || p.Number != 3 {
		t.Fatalf("by URL: %v %v", p, err)
	}
	if _, err := pickPull(list, "9"); err == nil {
		t.Fatal("found #9")
	}
}
