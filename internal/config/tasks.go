package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TaskKinds are the task trackers fleet can take work from (package
// tasks).
var TaskKinds = []string{"rome"}

// DefaultTaskMaxAgents is how many agents of one task source may work at
// the same time when max_agents is not set.
const DefaultTaskMaxAgents = 3

// DefaultTaskCloseAfter is how long an agent whose tickets are all done
// stays idle before fleet closes it, when close_after is not set.
const DefaultTaskCloseAfter = time.Hour

// TaskSource is a [[tasks]] entry: a task tracker (Rome; later Jira or
// Linear) fleet takes work from, out of its account's queue (Rome: a pull
// webhook), a batch at a time while it has agents to spare. Tickets
// assigned to the account in a project listed under [[tasks.project]]
// start an agent in that project's folder, one per project and batch;
// news on a ticket (an answer, a mention, a decision) goes to the agent
// working it.
//
//	[[tasks]]
//	kind = "rome"
//	url = "https://rome.example.com"
//	token_file = "~/.config/rome/fleet-token"
//
//	[[tasks.project]]
//	key = "ROME"
//	path = "/srv/rome"
type TaskSource struct {
	// Name identifies the source in fleet (agents, `fleet tasks`). Default:
	// Kind, which must then be unique.
	Name string `toml:"name,omitempty"`
	// Kind is the tracker: one of TaskKinds.
	Kind string `toml:"kind"`
	// URL is the tracker's base URL (Rome: its ROME_PUBLIC_URL).
	URL string `toml:"url"`
	// The account's API token comes from TokenFile (a file holding only the
	// token; "~/" is expanded), else TokenEnv (an environment variable of
	// the daemon), else Token. Prefer a file: config.toml is not a secret
	// store.
	TokenFile string `toml:"token_file,omitempty"`
	TokenEnv  string `toml:"token_env,omitempty"`
	Token     string `toml:"token,omitempty"`
	// Webhook is the queue fleet receives from: Rome's pull webhook
	// ("whk_…"). "" uses the account's only pull webhook (`fleet tasks
	// setup` creates one).
	Webhook string `toml:"webhook,omitempty"`
	// Disabled stops taking work from the source without forgetting it.
	Disabled bool `toml:"disabled,omitempty"`
	// MaxAgents caps how many of its agents may work at the same time: the
	// next batch is received only while fewer work. Default
	// DefaultTaskMaxAgents.
	MaxAgents int `toml:"max_agents,omitempty"`
	// CloseAfter is how long an agent whose tickets are all done (Done or
	// Cancelled) may stay idle before fleet closes it, as the dashboard's
	// Archive does: a duration ("1h", "30m"), or "off" to leave such agents
	// open. Default DefaultTaskCloseAfter.
	CloseAfter string `toml:"close_after,omitempty"`
	// TaskAgent holds the defaults for every project of the source.
	TaskAgent
	Projects []TaskProject `toml:"project"`
}

// TaskAgent is how agents for tickets are started.
type TaskAgent struct {
	// Adapter is the agent CLI. Default "claude".
	Adapter string `toml:"adapter,omitempty"`
	// Model and Effort, "" for the CLI's default.
	Model  string `toml:"model,omitempty"`
	Effort string `toml:"effort,omitempty"`
	// Isolation: "worktree" or "pinned"; "" follows default_isolation.
	Isolation string `toml:"isolation,omitempty"`
	// Sandbox: "none" or "docker"; "" follows [sandbox] default.
	Sandbox string `toml:"sandbox,omitempty"`
	// Args are extra arguments for the agent CLI, after [adapter.<id>]
	// args, e.g. ["--permission-mode", "acceptEdits"]. A project's replace
	// its source's.
	Args []string `toml:"args,omitempty"`
	// Instructions are told to the agent next to fleet's own: what this
	// team expects (run the tests, open a pull request, ...). A project's
	// come after its source's.
	Instructions string `toml:"instructions,omitempty"`
}

// TaskProject maps a project of the tracker to the folder its tickets are
// worked in.
type TaskProject struct {
	// Key is the project's key in the tracker, e.g. "ROME".
	Key string `toml:"key"`
	// Path is the folder: absolute (it must be inside a root), or relative
	// to Root. "~/" is expanded.
	Path string `toml:"path,omitempty"`
	// Root is a root's name; with it Path is relative ("" for the root
	// itself).
	Root string `toml:"root,omitempty"`
	// TaskAgent overrides the source's settings for this project; its
	// Instructions are added to the source's.
	TaskAgent
}

// projectKey is a tracker project key: Rome's, Jira's and Linear's all fit.
var projectKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

// SourceName returns Name, or Kind when it is unset.
func (t TaskSource) SourceName() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Kind
}

// MaxAgentsOrDefault returns MaxAgents or DefaultTaskMaxAgents.
func (t TaskSource) MaxAgentsOrDefault() int {
	if t.MaxAgents <= 0 {
		return DefaultTaskMaxAgents
	}
	return t.MaxAgents
}

// CloseAfterOrDefault returns CloseAfter as a duration:
// DefaultTaskCloseAfter when it is unset, 0 for "off" (never close).
func (t TaskSource) CloseAfterOrDefault() time.Duration {
	d, err := parseCloseAfter(t.CloseAfter)
	if err != nil {
		return 0 // checkTasks refuses it; keeping agents open is the safe side
	}
	return d
}

func parseCloseAfter(v string) (time.Duration, error) {
	switch v = strings.TrimSpace(v); v {
	case "":
		return DefaultTaskCloseAfter, nil
	case "off":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("close_after is a duration such as \"1h\" or \"30m\", or \"off\", not %q", v)
	}
	return d, nil
}

// Project returns the project with key (case-insensitive).
func (t TaskSource) Project(key string) (TaskProject, bool) {
	for _, p := range t.Projects {
		if strings.EqualFold(p.Key, key) {
			return p, true
		}
	}
	return TaskProject{}, false
}

// Agent returns how agents for project p are started: p's settings over
// the source's, instructions of both.
func (t TaskSource) Agent(p TaskProject) TaskAgent {
	a := t.TaskAgent
	if p.Adapter != "" {
		a.Adapter = p.Adapter
	}
	if p.Model != "" {
		a.Model, a.Effort = p.Model, ""
	}
	if p.Effort != "" {
		a.Effort = p.Effort
	}
	if p.Isolation != "" {
		a.Isolation = p.Isolation
	}
	if p.Sandbox != "" {
		a.Sandbox = p.Sandbox
	}
	if p.Args != nil {
		a.Args = p.Args
	}
	a.Instructions = strings.TrimSpace(strings.TrimSpace(t.Instructions) + "\n\n" + strings.TrimSpace(p.Instructions))
	if a.Adapter == "" {
		a.Adapter = "claude"
	}
	return a
}

// Secret returns the API token: from TokenFile, TokenEnv or Token.
func (t TaskSource) Secret() (string, error) {
	switch {
	case t.TokenFile != "":
		b, err := os.ReadFile(expandHome(t.TokenFile))
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("token_file %s is empty", t.TokenFile)
		}
		return tok, nil
	case t.TokenEnv != "":
		tok := strings.TrimSpace(os.Getenv(t.TokenEnv))
		if tok == "" {
			return "", fmt.Errorf("token_env: $%s is not set", t.TokenEnv)
		}
		return tok, nil
	case t.Token != "":
		return strings.TrimSpace(t.Token), nil
	}
	return "", errors.New("no token: set token_file, token_env or token")
}

// expandHome expands a leading "~/".
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// ProjectDir returns the Resolve arguments for p's folder: a root name and
// a relative path, or an absolute path.
func (p TaskProject) ProjectDir() (root, rel, abs string) {
	if p.Root != "" {
		return p.Root, p.Path, ""
	}
	return "", "", expandHome(p.Path)
}

// checkTasks validates the [[tasks]] entries.
func (c *Config) checkTasks() error {
	names := map[string]bool{}
	for i, t := range c.Tasks {
		where := fmt.Sprintf("[[tasks]] #%d", i+1)
		if !contains(TaskKinds, t.Kind) {
			return fmt.Errorf("%s: kind must be one of %s, not %q", where, strings.Join(TaskKinds, ", "), t.Kind)
		}
		name := t.SourceName()
		if names[name] {
			return fmt.Errorf("%s: a second source named %q; give each a name", where, name)
		}
		names[name] = true
		if !strings.HasPrefix(t.URL, "https://") && !strings.HasPrefix(t.URL, "http://") {
			return fmt.Errorf("%s (%s): url must start with https:// or http://, not %q", where, name, t.URL)
		}
		if _, err := parseCloseAfter(t.CloseAfter); err != nil {
			return fmt.Errorf("%s (%s): %w", where, name, err)
		}
		if err := checkTaskAgent(t.TaskAgent); err != nil {
			return fmt.Errorf("%s (%s): %w", where, name, err)
		}
		keys := map[string]bool{}
		for _, p := range t.Projects {
			if !projectKey.MatchString(p.Key) {
				return fmt.Errorf("%s (%s): project key %q is not a project key", where, name, p.Key)
			}
			k := strings.ToUpper(p.Key)
			if keys[k] {
				return fmt.Errorf("%s (%s): project %s is listed twice", where, name, p.Key)
			}
			keys[k] = true
			switch {
			case p.Root == "" && p.Path == "":
				return fmt.Errorf("%s (%s): project %s needs a path or a root", where, name, p.Key)
			case p.Root == "" && !filepath.IsAbs(expandHome(p.Path)):
				return fmt.Errorf("%s (%s): project %s: path must be absolute (or relative to root), not %q", where, name, p.Key, p.Path)
			case p.Root != "" && filepath.IsAbs(p.Path):
				return fmt.Errorf("%s (%s): project %s: with root, path is relative to it, not %q", where, name, p.Key, p.Path)
			}
			if err := checkTaskAgent(p.TaskAgent); err != nil {
				return fmt.Errorf("%s (%s): project %s: %w", where, name, p.Key, err)
			}
		}
	}
	return nil
}

func checkTaskAgent(a TaskAgent) error {
	switch a.Isolation {
	case "", "worktree", "pinned":
	default:
		return fmt.Errorf("isolation must be \"worktree\" or \"pinned\", not %q", a.Isolation)
	}
	switch a.Sandbox {
	case "", "none", "docker":
	default:
		return fmt.Errorf("sandbox must be \"none\" or \"docker\", not %q", a.Sandbox)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
