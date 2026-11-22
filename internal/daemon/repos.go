package daemon

// GitHub repositories to start a project from: the daemon lists what the
// host's GitHub CLI sees (ListReposRequest) and clones one into the work
// folder as a new root (CloneRepoRequest), so a client can start agents in
// a project that is not on the machine yet. gh runs as the daemon's user,
// with its sign-in and its git protocol, and never prompts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
)

const (
	// repoListMax and repoSearchMax are how many repositories a listing
	// and a search return.
	repoListMax   = 100
	repoSearchMax = 30
	// maxRepoQuery caps a search query.
	maxRepoQuery = 256
)

// repoHost is what the daemon asks about repositories (ghRepos; a fake in
// tests).
type repoHost interface {
	// login is the account gh is signed in as.
	login(ctx context.Context) (string, error)
	// list is the account's repositories, most recently pushed first;
	// search finds repositories on GitHub.
	list(ctx context.Context) ([]*fleetv1.Repo, error)
	search(ctx context.Context, q string) ([]*fleetv1.Repo, error)
	// clone clones repo ("owner/name") into path, which does not exist
	// (its folder does).
	clone(ctx context.Context, repo, path string) error
}

// repoHub is what asks GitHub about repositories; a variable for tests.
var repoHub repoHost = ghRepos{}

// ghRepos asks GitHub through the host's GitHub CLI (see ghCLI).
type ghRepos struct{}

// ghRepo is a repository as GitHub's REST API describes it.
type ghRepo struct {
	FullName    string    `json:"full_name"`
	Description string    `json:"description"`
	Private     bool      `json:"private"`
	Fork        bool      `json:"fork"`
	Archived    bool      `json:"archived"`
	PushedAt    time.Time `json:"pushed_at"`
	HTMLURL     string    `json:"html_url"`
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (r ghRepo) proto() *fleetv1.Repo {
	out := &fleetv1.Repo{
		FullName: r.FullName, Description: r.Description, Private: r.Private, Fork: r.Fork,
		Archived: r.Archived, Url: r.HTMLURL, CanPush: r.Permissions.Push,
	}
	if !r.PushedAt.IsZero() {
		out.PushedAtMs = r.PushedAt.UnixMilli()
	}
	return out
}

func repoProtos(list []ghRepo) []*fleetv1.Repo {
	out := make([]*fleetv1.Repo, 0, len(list))
	for _, r := range list {
		if r.FullName != "" {
			out = append(out, r.proto())
		}
	}
	return out
}

func (ghRepos) login(ctx context.Context) (string, error) {
	out, err := ghCLI{}.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", ghSignInError(err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (ghRepos) list(ctx context.Context) ([]*fleetv1.Repo, error) {
	out, err := ghCLI{}.run(ctx, "api", fmt.Sprintf("user/repos?per_page=%d&sort=pushed&affiliation=owner,collaborator,organization_member", repoListMax))
	if err != nil {
		return nil, ghSignInError(err)
	}
	var list []ghRepo
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("gh api user/repos: %w", err)
	}
	return repoProtos(list), nil
}

func (ghRepos) search(ctx context.Context, q string) ([]*fleetv1.Repo, error) {
	// -f puts the query into the URL of a GET: it is never read as a flag.
	out, err := ghCLI{}.run(ctx, "api", "-X", "GET", "search/repositories", "-f", "q="+q, "-f", fmt.Sprintf("per_page=%d", repoSearchMax))
	if err != nil {
		return nil, ghSignInError(err)
	}
	var res struct {
		Items []ghRepo `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("gh api search/repositories: %w", err)
	}
	return repoProtos(res.Items), nil
}

func (ghRepos) clone(ctx context.Context, repo, path string) error {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return errf(codeUnavailable, "gh (the GitHub CLI) is not installed on this host")
	}
	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "repo", "clone", repo, path)
	cmd.Dir = filepath.Dir(path)
	// Its own session: no terminal for ssh or a credential helper to ask
	// on, so a missing key fails instead of hanging.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1",
		"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errf(codeInvalid, "cloning %s took longer than %s", repo, cloneTimeout)
		}
		msg := gitMessage(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return ghSignInError(errf(codeInvalid, "gh repo clone %s: %s", repo, msg))
	}
	return nil
}

// ghSignInError turns gh's "run gh auth login" into an UNAVAILABLE error
// that says so plainly.
func ghSignInError(err error) error {
	code, msg := errorCode(err)
	if code != codeUnavailable && (strings.Contains(msg, "gh auth login") || strings.Contains(msg, "GH_TOKEN")) {
		return errf(codeUnavailable, "gh is not signed in to GitHub on this host: run `gh auth login` there")
	}
	return err
}

var (
	// repoNameRE is "owner/name" as GitHub allows them.
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)
	// scpGitHubRE is git@github.com:owner/name(.git).
	scpGitHubRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+@github\.com:(.+)$`)
)

// repoFullName is "owner/name" for a repository given as such, or as its
// github.com URL (https, ssh or git@github.com:...); ok is false for
// anything else.
func repoFullName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	p := s
	if m := scpGitHubRE.FindStringSubmatch(s); m != nil {
		p = m[1]
	} else {
		if !strings.Contains(s, "://") && strings.HasPrefix(strings.ToLower(s), "github.com/") {
			s = "https://" + s
		}
		if strings.Contains(s, "://") {
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh") || !strings.EqualFold(u.Hostname(), "github.com") {
				return "", false
			}
			// A page of the repository (…/tree/main, …/pull/7) names it too.
			parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 3)
			if len(parts) < 2 {
				return "", false
			}
			p = parts[0] + "/" + parts[1]
		}
	}
	p = strings.TrimSuffix(p, ".git")
	if !repoNameRE.MatchString(p) || strings.HasSuffix(p, "/.") || strings.HasSuffix(p, "/..") {
		return "", false
	}
	return p, true
}

// listRepos answers a ListReposRequest.
func (d *daemon) listRepos(ctx context.Context, req *fleetv1.ListReposRequest) (*fleetv1.ListReposResponse, error) {
	q := strings.TrimSpace(req.GetQuery())
	if len(q) > maxRepoQuery {
		return nil, errf(codeInvalid, "search query too long (%d bytes, max %d)", len(q), maxRepoQuery)
	}
	var login string
	var loginErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		login, loginErr = repoHub.login(ctx)
	}()
	var repos []*fleetv1.Repo
	var err error
	if q == "" {
		repos, err = repoHub.list(ctx)
	} else {
		repos, err = repoHub.search(ctx, q)
	}
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if loginErr != nil {
		d.log.Debug("gh login unknown", "err", loginErr)
	}
	cfg := d.config()
	work := cfg.WorkDirOrHome()
	local := newCloneFinder(ctx, cfg, work)
	for _, r := range repos {
		r.LocalPath = local.find(r.FullName)
	}
	return &fleetv1.ListReposResponse{Repos: repos, Login: login, WorkDir: work}, nil
}

// cloneFinder finds the folders that are clones of a repository: a root,
// or <work folder>/<name>.
type cloneFinder struct {
	ctx  context.Context
	work string
	// roots are the repositories of the roots, by lower-case full name.
	roots map[string]string
	// names are the folders in work, by lower-case name: GitHub names
	// are not case-sensitive.
	names map[string]string
}

func newCloneFinder(ctx context.Context, cfg config.Config, work string) *cloneFinder {
	f := &cloneFinder{ctx: ctx, work: work, roots: map[string]string{}, names: map[string]string{}}
	for _, r := range cfg.Roots {
		if name := cloneOf(ctx, r.Path); name != "" {
			if _, dup := f.roots[name]; !dup {
				f.roots[name] = r.Path
			}
		}
	}
	if work != "" {
		ents, _ := os.ReadDir(work)
		for _, e := range ents {
			if e.IsDir() {
				f.names[strings.ToLower(e.Name())] = e.Name()
			}
		}
	}
	return f
}

// find is a folder that is a clone of fullName, "" if there is none.
func (f *cloneFinder) find(fullName string) string {
	key := strings.ToLower(fullName)
	if p, ok := f.roots[key]; ok {
		return p
	}
	_, name, _ := strings.Cut(key, "/")
	if dir, ok := f.names[name]; ok {
		if p := filepath.Join(f.work, dir); cloneOf(f.ctx, p) == key {
			return p
		}
	}
	return ""
}

// cloneOf is the lower-case "owner/name" of the GitHub repository whose
// clone dir is (its remote origin), "" when it is none.
func cloneOf(ctx context.Context, dir string) string {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	u, err := runGit(ctx, dir, gitReadTimeout, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	name, ok := repoFullName(u)
	if !ok {
		return ""
	}
	return strings.ToLower(name)
}

// cloneRepo answers a CloneRepoRequest.
func (d *daemon) cloneRepo(ctx context.Context, req *fleetv1.CloneRepoRequest) (*fleetv1.CloneRepoResponse, error) {
	repo, ok := repoFullName(req.GetRepo())
	if !ok {
		return nil, errf(codeInvalid, "%q is no GitHub repository: use owner/name or its github.com URL", req.GetRepo())
	}
	for _, a := range req.GetAdapters() {
		if _, ok := d.opts.Adapters.Get(a); !ok {
			return nil, errf(codeNotFound, "unknown adapter %q", a)
		}
	}
	work := d.workDir()
	path := ""
	if strings.TrimSpace(req.GetPath()) == "" {
		// Already cloned (a root, or in the work folder under any case).
		path = newCloneFinder(ctx, d.config(), work).find(repo)
	}
	if path == "" {
		var err error
		if path, err = clonePath(req.GetPath(), work, repo); err != nil {
			return nil, err
		}
	}
	// One clone per folder at a time: a second request waits and finds
	// the clone the first one made.
	l := d.agents.gitLock(path)
	l.Lock()
	defer l.Unlock()
	existed := false
	switch fi, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, errf(codeInvalid, "%s: %v", path, err)
	case !fi.IsDir():
		return nil, errf(codeExists, "%s already exists and is not a folder", path)
	case cloneOf(ctx, path) == strings.ToLower(repo):
		existed = true
	case !emptyDir(path):
		return nil, errf(codeExists, "%s already exists: choose another folder", path)
	}
	if !existed {
		if err := cloneInto(ctx, repo, path); err != nil {
			return nil, err
		}
		d.log.Info("repository cloned", "repo", repo, "path", path)
	}
	cfg := d.config()
	if r, ok := cfg.RootByPath(path); ok {
		return &fleetv1.CloneRepoResponse{Root: r.Proto(), Existed: existed}, nil
	}
	root, err := d.addRoot(&fleetv1.AddRootRequest{Path: path, Adapters: req.GetAdapters(), Trust: req.GetTrust()})
	if err != nil {
		return nil, err
	}
	return &fleetv1.CloneRepoResponse{Root: root, Existed: existed}, nil
}

// cloneInto clones repo into a hidden folder beside path and renames it
// to path once complete, so a clone cut short never looks like one. It
// goes on when the client stops waiting (cloneTimeout bounds it).
func cloneInto(ctx context.Context, repo, path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return errf(codeInvalid, "make %s: %v", parent, err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(path)+".clone-")
	if err != nil {
		return errf(codeInvalid, "clone into %s: %v", parent, err)
	}
	defer os.RemoveAll(tmp) // gone after the rename
	dst := filepath.Join(tmp, "repo")
	if err := repoHub.clone(context.WithoutCancel(ctx), repo, dst); err != nil {
		return err
	}
	_ = os.Remove(path) // an empty folder in the way
	if err := os.Rename(dst, path); err != nil {
		return errf(codeInvalid, "move the clone to %s: %v", path, err)
	}
	return nil
}

// clonePath is where a clone of repo goes: path (absolute or "~/..."),
// else <work>/<name>; absolute and clean, with the folders leading to it
// symlink-resolved.
func clonePath(path, work, repo string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		if work == "" {
			return "", errf(codeInvalid, "no work folder to clone %s into: give a path", repo)
		}
		_, name, _ := strings.Cut(repo, "/")
		path = filepath.Join(work, name)
	} else if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errf(codeInvalid, "no home directory to put %s in", path)
		}
		path = filepath.Join(home, path[1:])
	}
	if !filepath.IsAbs(path) {
		return "", errf(codeInvalid, "%q is not an absolute path", path)
	}
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return "", errf(codeInvalid, "cannot clone into /")
	}
	// The deepest folder that exists, resolved, with the rest on top.
	dir, rest := path, ""
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// emptyDir reports whether dir is a folder with nothing in it.
func emptyDir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return len(names) == 0 && err != nil
}
