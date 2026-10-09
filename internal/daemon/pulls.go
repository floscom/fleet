package daemon

// GitHub pull requests agents create: the daemon follows each agent's
// transcript for a `gh pr create` (or a GitHub MCP tool that creates one),
// keeps the pull requests it made with the agent (Agent.pull_requests),
// looks up their status with the host's GitHub CLI, and merges them on
// request (MergePullRequestRequest), so a client can show what an agent
// proposes and accept it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/transcript"
)

// Variables for tests.
var (
	// pullInterval is how often transcripts are read for new pull requests
	// and due ones looked up.
	pullInterval = 10 * time.Second
	// pullRefresh is how often an open pull request of a live agent is
	// looked up; pullRefreshIdle the same for a finished agent.
	pullRefresh     = time.Minute
	pullRefreshIdle = 10 * time.Minute
)

const (
	// pullScan bounds the transcript bytes read per round, over all agents:
	// the first round after an upgrade reads every agent's history.
	pullScan = 32 << 20
	// pullLookups bounds the lookups per round.
	pullLookups = 4
	// maxPulls is how many pull requests an agent keeps, the newest.
	maxPulls = 20
	// ghTimeout bounds one gh command; mergeTimeout a whole merge.
	ghTimeout    = 30 * time.Second
	mergeTimeout = 90 * time.Second
)

// prScan is how far an agent's transcript was read for pull requests.
type prScan struct {
	Path string `json:"path"`
	Off  int64  `json:"off"`
	// Pending are the tool calls that create a pull request whose results
	// have not been read yet, by tool call ID.
	Pending []string `json:"pending,omitempty"`
}

// prCreateRe finds `gh pr create` in a shell command.
var prCreateRe = regexp.MustCompile(`(?:^|[\s;&|(])gh\s+pr\s+create\b`)

// prURLRe finds pull request URLs: https://HOST/OWNER/NAME/pull/N.
var prURLRe = regexp.MustCompile(`https://([A-Za-z0-9.-]+)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]{1,9})\b`)

// createsPR reports whether tool call e creates a pull request.
func createsPR(e transcript.Entry) bool {
	name := strings.ToLower(e.Name)
	if strings.Contains(name, "create_pull_request") || strings.Contains(name, "create-pull-request") {
		return true
	}
	return prCreateRe.MatchString(e.Text) || prCreateRe.MatchString(e.Detail)
}

// prRef is a pull request named by its URL.
type prRef struct {
	url, host, repo string
	number          int
}

// ghRepo names the repository for gh -R: OWNER/NAME on github.com,
// HOST/OWNER/NAME elsewhere.
func (r prRef) ghRepo() string {
	if r.host == "github.com" {
		return r.repo
	}
	return r.host + "/" + r.repo
}

// prRefs returns the distinct pull request URLs in s, in order.
func prRefs(s string) []prRef {
	var out []prRef
	for _, m := range prURLRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[4])
		r := prRef{host: strings.ToLower(m[1]), repo: m[2] + "/" + m[3], number: n}
		r.url = "https://" + r.host + "/" + r.repo + "/pull/" + m[4]
		if n > 0 && !slices.ContainsFunc(out, func(o prRef) bool { return o.url == r.url }) {
			out = append(out, r)
		}
	}
	return out
}

// parsePRURL parses a pull request URL.
func parsePRURL(s string) (prRef, bool) {
	refs := prRefs(s)
	if len(refs) != 1 || refs[0].url != strings.TrimSuffix(strings.TrimSpace(s), "/") {
		return prRef{}, false
	}
	return refs[0], true
}

// foundPR is a pull request a transcript says was created.
type foundPR struct {
	ref    prRef
	timeMs int64
}

// read takes the entries of the next lines of the transcript and returns
// the pull requests they show created.
func (s *prScan) read(entries []transcript.Entry) []foundPR {
	var out []foundPR
	add := func(output string, ts int64) {
		for _, r := range prRefs(output) {
			out = append(out, foundPR{ref: r, timeMs: ts})
		}
	}
	for _, e := range entries {
		switch e.Kind {
		case transcript.Tool:
			if !createsPR(e) {
				continue
			}
			if e.ID == "" || e.Output != "" {
				add(e.Output, e.TimeMs)
			} else if !slices.Contains(s.Pending, e.ID) {
				s.Pending = append(s.Pending, e.ID)
				if len(s.Pending) > 32 {
					s.Pending = s.Pending[1:]
				}
			}
		case transcript.Result:
			if i := slices.Index(s.Pending, e.ID); i >= 0 {
				s.Pending = slices.Delete(s.Pending, i, i+1)
				add(e.Output, e.TimeMs)
			}
		}
	}
	return out
}

// pullLoop follows agents for pull requests until ctx is cancelled.
func (m *manager) pullLoop(ctx context.Context) {
	t := time.NewTicker(pullInterval)
	defer t.Stop()
	for {
		m.watchPulls(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchPulls reads what agents' transcripts gained for new pull requests,
// then looks up the pull requests that are due.
func (m *manager) watchPulls(ctx context.Context) {
	budget := int64(pullScan)
	var ids []string
	m.mu.Lock()
	for _, a := range m.sortedLocked() {
		if !a.busy && (a.Transcript != "" || a.live()) {
			ids = append(ids, a.ID)
		}
	}
	m.mu.Unlock()
	read := false
	for _, id := range ids {
		if ctx.Err() != nil || budget <= 0 {
			break
		}
		n := m.scanPulls(id, budget)
		budget -= n
		read = read || n > 0
	}
	if read {
		// The scan offsets; agents with new pull requests were saved already.
		m.mu.Lock()
		m.saveLocked()
		m.mu.Unlock()
	}
	m.lookUpPulls(ctx, time.Now())
}

// scanPulls reads agent id's transcript from where the last scan stopped,
// at most about limit bytes, adds the pull requests it finds, and returns
// how many bytes it read. The new offset is saved with the next save.
func (m *manager) scanPulls(id string, limit int64) int64 {
	s, err := m.session(id)
	if err != nil || s.t == nil || s.path == "" {
		return 0
	}
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return 0
	}
	var scan prScan
	if a.PRScan != nil && a.PRScan.Path == s.path {
		scan = *a.PRScan
		scan.Pending = slices.Clone(scan.Pending)
	} else {
		scan = prScan{Path: s.path}
	}
	m.mu.Unlock()

	if fi, err := os.Stat(s.path); err != nil || fi.Size() == scan.Off {
		return 0
	} else if fi.Size() < scan.Off {
		scan = prScan{Path: s.path} // replaced
	}
	start := scan.Off
	var found []foundPR
	for scan.Off-start < limit {
		p, err := transcript.Forward(s.path, scan.Off, s.t.ParseTranscript)
		if errors.Is(err, transcript.ErrShrunk) {
			scan, start, found = prScan{Path: s.path}, 0, nil
			continue
		}
		if err != nil {
			m.d.log.Warn("reading transcript for pull requests failed", "agent", id, "err", err)
			break
		}
		found = append(found, scan.read(p.Entries)...)
		scan.Off = p.End
		if !p.More {
			break
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok = m.agents[id]
	if !ok {
		return scan.Off - start
	}
	a.PRScan = &scan
	added := false
	for _, f := range found {
		if slices.ContainsFunc(a.PullRequests, func(p *fleetv1.PullRequest) bool { return p.Url == f.ref.url }) {
			continue
		}
		a.PullRequests = append(a.PullRequests, &fleetv1.PullRequest{
			Url: f.ref.url, Repo: f.ref.repo, Number: int32(f.ref.number), CreatedAtMs: f.timeMs,
		})
		added = true
	}
	if n := len(a.PullRequests); n > maxPulls {
		a.PullRequests = slices.Clone(a.PullRequests[n-maxPulls:])
	}
	if added {
		m.changedLocked(a)
	}
	return scan.Off - start
}

// pullDue reports whether pull request p of an agent (live or not) is due
// for a lookup at now.
func pullDue(p *fleetv1.PullRequest, live bool, now time.Time) bool {
	switch p.State {
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED, fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED:
		return false
	}
	every := pullRefresh
	if !live {
		every = pullRefreshIdle
	}
	return now.Sub(time.UnixMilli(p.CheckedAtMs)) >= every
}

// lookUpPulls looks up the pull requests that are due, at most pullLookups,
// those never looked up and those of live agents first.
func (m *manager) lookUpPulls(ctx context.Context, now time.Time) {
	type due struct {
		id, url string
		rank    int
	}
	var list []due
	m.mu.Lock()
	for _, a := range m.sortedLocked() {
		for _, p := range a.PullRequests {
			if !pullDue(p, a.live(), now) {
				continue
			}
			rank := 2
			if p.CheckedAtMs == 0 {
				rank = 0
			} else if a.live() {
				rank = 1
			}
			list = append(list, due{a.ID, p.Url, rank})
		}
	}
	m.mu.Unlock()
	slices.SortStableFunc(list, func(a, b due) int { return a.rank - b.rank })
	for i, d := range list {
		if i == pullLookups || ctx.Err() != nil {
			return
		}
		m.lookUpPull(ctx, d.id, d.url)
	}
}

// lookUpPull looks up pull request url of agent id on GitHub and stores
// what it found; a failed lookup keeps the last status, with the error
// as its detail.
func (m *manager) lookUpPull(ctx context.Context, id, url string) *fleetv1.PullRequest {
	start := time.Now()
	ref, _ := parsePRURL(url)
	st, err := m.github().view(ctx, ref)
	if err == nil {
		st.MergeMethods = m.mergeMethods(ctx, ref)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil
	}
	i := slices.IndexFunc(a.PullRequests, func(p *fleetv1.PullRequest) bool { return p.Url == url })
	if i < 0 {
		return nil
	}
	old := a.PullRequests[i]
	if old.CheckedAtMs > start.UnixMilli() {
		// A later lookup (after a merge) got there first.
		return proto.Clone(old).(*fleetv1.PullRequest)
	}
	next := proto.Clone(old).(*fleetv1.PullRequest)
	if err != nil {
		next.Detail = err.Error()
	} else {
		next = st
		next.Url, next.Repo, next.Number, next.CreatedAtMs = old.Url, old.Repo, old.Number, old.CreatedAtMs
	}
	next.CheckedAtMs = time.Now().UnixMilli()
	a.PullRequests[i] = next
	if proto.Equal(withoutCheck(old), withoutCheck(next)) {
		m.saveLocked()
	} else {
		m.changedLocked(a)
	}
	return proto.Clone(next).(*fleetv1.PullRequest)
}

// withoutCheck is p without the time it was looked up, to tell whether a
// lookup changed anything clients show.
func withoutCheck(p *fleetv1.PullRequest) *fleetv1.PullRequest {
	c := proto.Clone(p).(*fleetv1.PullRequest)
	c.CheckedAtMs = 0
	return c
}

// mergeMethods returns the merge methods repository ref allows, preferred
// first; nil if unknown. They are asked once per repository and daemon run.
func (m *manager) mergeMethods(ctx context.Context, ref prRef) []fleetv1.MergeMethod {
	key := ref.ghRepo()
	m.pullMu.Lock()
	ms, ok := m.repoMethods[key]
	m.pullMu.Unlock()
	if ok {
		return ms
	}
	ms, err := m.github().methods(ctx, ref)
	if err != nil {
		return nil
	}
	m.pullMu.Lock()
	if m.repoMethods == nil {
		m.repoMethods = map[string][]fleetv1.MergeMethod{}
	}
	m.repoMethods[key] = ms
	m.pullMu.Unlock()
	return ms
}

// mergePull merges a pull request agent ref created.
func (m *manager) mergePull(ctx context.Context, req *fleetv1.MergePullRequestRequest) (*fleetv1.PullRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()
	m.mu.Lock()
	a, err := m.find(req.GetAgent())
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	id, name := a.ID, a.Name
	p, err := pickPull(a.PullRequests, req.GetPullRequest())
	if err != nil {
		m.mu.Unlock()
		code, msg := errorCode(err)
		return nil, errf(code, "%s: %s", name, msg)
	}
	p = proto.Clone(p).(*fleetv1.PullRequest)
	m.mu.Unlock()

	ref, ok := parsePRURL(p.Url)
	if !ok {
		return nil, errf(codeInternal, "bad pull request URL %q", p.Url)
	}
	// Look again: it may have been merged or changed since.
	if cur := m.lookUpPull(ctx, id, p.Url); cur != nil && cur.Detail == "" {
		p = cur
	}
	switch p.State {
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED:
		return nil, errf(codeInvalid, "#%d is merged already", p.Number)
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED:
		return nil, errf(codeInvalid, "#%d is closed", p.Number)
	}
	method := req.GetMethod()
	if method == fleetv1.MergeMethod_MERGE_METHOD_UNSPECIFIED {
		method = fleetv1.MergeMethod_MERGE_METHOD_SQUASH
		if len(p.MergeMethods) > 0 {
			method = p.MergeMethods[0]
		}
	} else if len(p.MergeMethods) > 0 && !slices.Contains(p.MergeMethods, method) {
		return nil, errf(codeInvalid, "%s does not allow %s merges", ref.repo, methodName(method))
	}
	gh := m.github()
	if p.Draft {
		if err := gh.ready(ctx, ref); err != nil {
			return nil, err
		}
	}
	if err := gh.merge(ctx, ref, method, req.GetDeleteBranch()); err != nil {
		m.lookUpPull(ctx, id, p.Url)
		return nil, err
	}
	m.d.log.Info("merged pull request", "agent", id, "url", p.Url, "method", methodName(method))
	if cur := m.lookUpPull(context.WithoutCancel(ctx), id, p.Url); cur != nil {
		return cur, nil
	}
	return p, nil
}

// pickPull finds the pull request of list named by which (a URL or a
// number), or the only open one if which is empty.
func pickPull(list []*fleetv1.PullRequest, which string) (*fleetv1.PullRequest, error) {
	which = strings.TrimPrefix(strings.TrimSpace(which), "#")
	if which != "" {
		n, numErr := strconv.Atoi(which)
		for _, p := range list {
			if p.Url == strings.TrimSuffix(which, "/") || numErr == nil && int(p.Number) == n {
				return p, nil
			}
		}
		return nil, errf(codeNotFound, "no pull request %q", which)
	}
	var open []*fleetv1.PullRequest
	for _, p := range list {
		switch p.State {
		case fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED, fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED:
		default:
			open = append(open, p)
		}
	}
	switch len(open) {
	case 0:
		return nil, errf(codeNotFound, "no open pull request")
	case 1:
		return open[0], nil
	}
	nums := make([]string, len(open))
	for i, p := range open {
		nums[i] = "#" + strconv.Itoa(int(p.Number))
	}
	return nil, errf(codeInvalid, "%d open pull requests (%s): say which", len(open), strings.Join(nums, ", "))
}

func methodName(m fleetv1.MergeMethod) string {
	switch m {
	case fleetv1.MergeMethod_MERGE_METHOD_SQUASH:
		return "squash"
	case fleetv1.MergeMethod_MERGE_METHOD_MERGE:
		return "merge"
	case fleetv1.MergeMethod_MERGE_METHOD_REBASE:
		return "rebase"
	}
	return "default"
}

// ---------------------------------------------------------------------------
// GitHub

// github is what the daemon asks GitHub (ghCLI; a fake in tests).
type github interface {
	// view looks up a pull request: everything but its URL, repo, number
	// and creation time, which the caller keeps.
	view(ctx context.Context, r prRef) (*fleetv1.PullRequest, error)
	// methods returns the merge methods a repository allows, preferred first.
	methods(ctx context.Context, r prRef) ([]fleetv1.MergeMethod, error)
	ready(ctx context.Context, r prRef) error
	merge(ctx context.Context, r prRef, method fleetv1.MergeMethod, deleteBranch bool) error
}

// gitHub is what asks GitHub; a variable for tests.
var gitHub github = ghCLI{}

func (m *manager) github() github { return gitHub }

// ghCLI asks GitHub through the host's GitHub CLI, logged in as whoever
// runs the daemon. It runs in the fleet home, outside any repository, so
// it never touches an agent's checkout.
type ghCLI struct{}

func (ghCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, errf(codeUnavailable, "gh (the GitHub CLI) is not installed on this host")
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = "gh timed out"
		}
		return nil, errf(codeInvalid, "%s", ghMessage(msg))
	}
	return out, nil
}

// ghMessage shortens what gh says on failure to its first lines.
func ghMessage(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	s = strings.Join(lines, " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// ghPull is what `gh pr view --json` says about a pull request.
type ghPull struct {
	Title             string    `json:"title"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	HeadRefName       string    `json:"headRefName"`
	BaseRefName       string    `json:"baseRefName"`
	MergeStateStatus  string    `json:"mergeStateStatus"`
	ReviewDecision    string    `json:"reviewDecision"`
	Additions         int32     `json:"additions"`
	Deletions         int32     `json:"deletions"`
	ChangedFiles      int32     `json:"changedFiles"`
	MergedAt          time.Time `json:"mergedAt"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
}

// ghCheck is a check run (Status, Conclusion) or a commit status (State).
type ghCheck struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

const ghPullFields = "title,state,isDraft,headRefName,baseRefName,mergeStateStatus,reviewDecision,additions,deletions,changedFiles,mergedAt,statusCheckRollup"

func (g ghCLI) view(ctx context.Context, r prRef) (*fleetv1.PullRequest, error) {
	out, err := g.run(ctx, "pr", "view", r.url, "--json", ghPullFields)
	if err != nil {
		return nil, err
	}
	var gp ghPull
	if err := json.Unmarshal(out, &gp); err != nil {
		return nil, fmt.Errorf("gh pr view: %w", err)
	}
	return gp.proto(), nil
}

func (gp ghPull) proto() *fleetv1.PullRequest {
	p := &fleetv1.PullRequest{
		Title: gp.Title, Draft: gp.IsDraft, HeadBranch: gp.HeadRefName, BaseBranch: gp.BaseRefName,
		MergeState: strings.ToLower(gp.MergeStateStatus), ReviewDecision: strings.ToLower(gp.ReviewDecision),
		Additions: gp.Additions, Deletions: gp.Deletions, ChangedFiles: gp.ChangedFiles,
		Checks: checksState(gp.StatusCheckRollup),
	}
	switch gp.State {
	case "OPEN":
		p.State = fleetv1.PullRequestState_PULL_REQUEST_STATE_OPEN
	case "MERGED":
		p.State = fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED
	case "CLOSED":
		p.State = fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED
	}
	if !gp.MergedAt.IsZero() {
		p.MergedAtMs = gp.MergedAt.UnixMilli()
	}
	return p
}

// checksState sums up a pull request's checks: failing if any failed,
// else pending if any is not done, else passing.
func checksState(checks []ghCheck) fleetv1.ChecksState {
	if len(checks) == 0 {
		return fleetv1.ChecksState_CHECKS_STATE_UNSPECIFIED
	}
	pending := false
	for _, c := range checks {
		res := c.State // commit status
		if res == "" {
			if c.Status != "COMPLETED" {
				pending = true
				continue
			}
			res = c.Conclusion
		}
		switch res {
		case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
			return fleetv1.ChecksState_CHECKS_STATE_FAILING
		case "PENDING", "EXPECTED", "":
			pending = true
		}
	}
	if pending {
		return fleetv1.ChecksState_CHECKS_STATE_PENDING
	}
	return fleetv1.ChecksState_CHECKS_STATE_PASSING
}

func (g ghCLI) methods(ctx context.Context, r prRef) ([]fleetv1.MergeMethod, error) {
	out, err := g.run(ctx, "repo", "view", r.ghRepo(), "--json", "squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed")
	if err != nil {
		return nil, err
	}
	var allowed struct {
		Squash bool `json:"squashMergeAllowed"`
		Merge  bool `json:"mergeCommitAllowed"`
		Rebase bool `json:"rebaseMergeAllowed"`
	}
	if err := json.Unmarshal(out, &allowed); err != nil {
		return nil, fmt.Errorf("gh repo view: %w", err)
	}
	ms := []fleetv1.MergeMethod{}
	if allowed.Squash {
		ms = append(ms, fleetv1.MergeMethod_MERGE_METHOD_SQUASH)
	}
	if allowed.Merge {
		ms = append(ms, fleetv1.MergeMethod_MERGE_METHOD_MERGE)
	}
	if allowed.Rebase {
		ms = append(ms, fleetv1.MergeMethod_MERGE_METHOD_REBASE)
	}
	return ms, nil
}

func (g ghCLI) ready(ctx context.Context, r prRef) error {
	_, err := g.run(ctx, "pr", "ready", strconv.Itoa(r.number), "-R", r.ghRepo())
	return err
}

// merge merges with -R, which keeps gh from deleting or switching local
// branches (the agent's worktree may have the branch checked out).
func (g ghCLI) merge(ctx context.Context, r prRef, method fleetv1.MergeMethod, deleteBranch bool) error {
	args := []string{"pr", "merge", strconv.Itoa(r.number), "-R", r.ghRepo(), "--" + methodName(method)}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	_, err := g.run(ctx, args...)
	return err
}

// clonePulls copies pull requests for a client.
func clonePulls(list []*fleetv1.PullRequest) []*fleetv1.PullRequest {
	if len(list) == 0 {
		return nil
	}
	out := make([]*fleetv1.PullRequest, len(list))
	for i, p := range list {
		out[i] = proto.Clone(p).(*fleetv1.PullRequest)
	}
	return out
}
