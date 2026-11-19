package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Editing [[tasks]] (fleet tasks add, map, set, ...). Each edit works on a
// copy: an edit that fails validation leaves the config as it was. None
// of them Save.

// ErrUnknownTaskSource is returned for a source name no [[tasks]] has.
var ErrUnknownTaskSource = errors.New("no task source")

// CloneTasks returns a deep copy of ts.
func CloneTasks(ts []TaskSource) []TaskSource {
	out := slices.Clone(ts)
	for i := range out {
		out[i].Args = slices.Clone(out[i].Args)
		out[i].Projects = slices.Clone(out[i].Projects)
		for j := range out[i].Projects {
			out[i].Projects[j].Args = slices.Clone(out[i].Projects[j].Args)
		}
	}
	return out
}

// editTasks applies fn to a copy of c.Tasks and keeps the result if it
// validates.
func (c *Config) editTasks(fn func(ts []TaskSource) ([]TaskSource, error)) error {
	ts, err := fn(CloneTasks(c.Tasks))
	if err != nil {
		return err
	}
	tmp := Config{Tasks: ts}
	if err := tmp.checkTasks(); err != nil {
		return err
	}
	c.Tasks = ts
	return nil
}

// taskSourceIndex finds source name in ts; "" names the only source.
func taskSourceIndex(ts []TaskSource, name string) (int, error) {
	if name == "" {
		switch len(ts) {
		case 0:
			return -1, fmt.Errorf("%w: add one with `fleet tasks add`", ErrUnknownTaskSource)
		case 1:
			return 0, nil
		}
		return -1, errors.New("there are several task sources; name one with --source")
	}
	for i, t := range ts {
		if t.SourceName() == name {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w %q", ErrUnknownTaskSource, name)
}

// TaskSourceIndex is the index of source name ("" for the only one) in
// c.Tasks.
func (c *Config) TaskSourceIndex(name string) (int, error) { return taskSourceIndex(c.Tasks, name) }

// AddTaskSource adds s ([[tasks]]); its name must be new.
func (c *Config) AddTaskSource(s TaskSource) error {
	if s.Kind == "" {
		s.Kind = "rome"
	}
	return c.editTasks(func(ts []TaskSource) ([]TaskSource, error) {
		for _, o := range ts {
			if o.SourceName() == s.SourceName() {
				return nil, fmt.Errorf("task source %q exists already", s.SourceName())
			}
		}
		return append(ts, s), nil
	})
}

// RemoveTaskSource removes source name.
func (c *Config) RemoveTaskSource(name string) error {
	return c.editTasks(func(ts []TaskSource) ([]TaskSource, error) {
		i, err := taskSourceIndex(ts, name)
		if err != nil {
			return nil, err
		}
		return slices.Delete(ts, i, i+1), nil
	})
}

// TaskProjectPatch changes a [[tasks.project]]: nil fields stay as they
// are (on a new project: unset).
type TaskProjectPatch struct {
	Path, Root                                 *string
	Adapter, Model, Effort, Isolation, Sandbox *string
	Instructions                               *string
	Args                                       *[]string
}

// MapTaskProject maps project key of source to a folder, or changes its
// mapping, and returns it and whether it is new. A new mapping needs Path
// or Root.
func (c *Config) MapTaskProject(source, key string, p TaskProjectPatch) (TaskProject, bool, error) {
	var out TaskProject
	var created bool
	err := c.editTasks(func(ts []TaskSource) ([]TaskSource, error) {
		i, err := taskSourceIndex(ts, source)
		if err != nil {
			return nil, err
		}
		projects := ts[i].Projects
		j := slices.IndexFunc(projects, func(o TaskProject) bool { return strings.EqualFold(o.Key, key) })
		if j < 0 {
			if p.Path == nil && p.Root == nil {
				return nil, fmt.Errorf("project %s is not mapped yet: give its folder", key)
			}
			projects = append(projects, TaskProject{Key: strings.ToUpper(key)})
			j, created = len(projects)-1, true
		}
		tp := &projects[j]
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = strings.TrimSpace(*v)
			}
		}
		if p.Path != nil && p.Root == nil {
			tp.Root = "" // a path alone is absolute
		}
		set(&tp.Path, p.Path)
		set(&tp.Root, p.Root)
		set(&tp.Adapter, p.Adapter)
		set(&tp.Model, p.Model)
		set(&tp.Effort, p.Effort)
		set(&tp.Isolation, p.Isolation)
		set(&tp.Sandbox, p.Sandbox)
		set(&tp.Instructions, p.Instructions)
		if p.Args != nil {
			tp.Args = slices.Clone(*p.Args)
			if len(tp.Args) == 0 {
				tp.Args = nil
			}
		}
		ts[i].Projects = projects
		out = *tp
		return ts, nil
	})
	return out, created, err
}

// UnmapTaskProject removes the mapping of project key of source.
func (c *Config) UnmapTaskProject(source, key string) error {
	return c.editTasks(func(ts []TaskSource) ([]TaskSource, error) {
		i, err := taskSourceIndex(ts, source)
		if err != nil {
			return nil, err
		}
		j := slices.IndexFunc(ts[i].Projects, func(o TaskProject) bool { return strings.EqualFold(o.Key, key) })
		if j < 0 {
			return nil, fmt.Errorf("project %s is not mapped in %s", key, ts[i].SourceName())
		}
		ts[i].Projects = slices.Delete(ts[i].Projects, j, j+1)
		return ts, nil
	})
}

// TaskOptions are the settings SetTaskOption changes.
var TaskOptions = []string{
	"url", "token_file", "token_env", "webhook", "disabled", "max_agents",
	"adapter", "model", "effort", "isolation", "sandbox", "args", "instructions",
}

// SetTaskOption sets option key (one of TaskOptions) of source to values:
// one value, none to unset it, or the arguments for "args".
func (c *Config) SetTaskOption(source, key string, values []string) error {
	return c.editTasks(func(ts []TaskSource) ([]TaskSource, error) {
		i, err := taskSourceIndex(ts, source)
		if err != nil {
			return nil, err
		}
		s := &ts[i]
		if key == "args" {
			s.Args = slices.Clone(values)
			if len(s.Args) == 0 {
				s.Args = nil
			}
			return ts, nil
		}
		if len(values) > 1 {
			return nil, fmt.Errorf("%s takes one value, not %d (quote it)", key, len(values))
		}
		v := ""
		if len(values) == 1 {
			v = strings.TrimSpace(values[0])
		}
		str := map[string]*string{
			"url": &s.URL, "webhook": &s.Webhook, "adapter": &s.Adapter, "model": &s.Model, "effort": &s.Effort,
			"isolation": &s.Isolation, "sandbox": &s.Sandbox, "instructions": &s.Instructions,
		}
		switch key {
		case "token_file", "token_env":
			// One way to get the token: setting one drops the others.
			s.TokenFile, s.TokenEnv, s.Token = "", "", ""
			if key == "token_file" {
				s.TokenFile = v
			} else {
				s.TokenEnv = v
			}
		case "disabled":
			b, err := strconv.ParseBool(cmpOr(v, "false"))
			if err != nil {
				return nil, fmt.Errorf("disabled is true or false, not %q", v)
			}
			s.Disabled = b
		case "max_agents":
			n := 0
			if v != "" {
				if n, err = strconv.Atoi(v); err != nil || n < 0 {
					return nil, fmt.Errorf("max_agents is a number of agents, not %q", v)
				}
			}
			s.MaxAgents = n
		default:
			p, ok := str[key]
			if !ok {
				return nil, fmt.Errorf("unknown option %q; options are %s", key, strings.Join(TaskOptions, ", "))
			}
			*p = v
		}
		return ts, nil
	})
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// SameConnection reports whether a and b reach the tracker the same way:
// a change of any of these restarts the source.
func SameConnection(a, b TaskSource) bool {
	return a.Kind == b.Kind && a.URL == b.URL && a.TokenFile == b.TokenFile && a.TokenEnv == b.TokenEnv &&
		a.Token == b.Token && a.Webhook == b.Webhook && a.Disabled == b.Disabled
}
