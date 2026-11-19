package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
)

// editTasks changes [[tasks]] (EditTasksRequest), saves config.toml and
// applies the change to the running sources.
func (d *daemon) editTasks(ctx context.Context, req *fleetv1.EditTasksRequest) (*fleetv1.EditTasksResponse, error) {
	d.cfgMu.Lock()
	before := config.CloneTasks(d.cfg.Tasks)
	sync, err := d.editTasksLocked(req)
	if err == nil {
		err = d.cfg.Save()
	}
	if err != nil {
		d.cfg.Tasks = before
		d.cfgMu.Unlock()
		if errors.Is(err, config.ErrUnknownTaskSource) {
			return nil, errf(codeNotFound, "%v", err)
		}
		var ae *apiError
		if errors.As(err, &ae) {
			return nil, err
		}
		return nil, errf(codeInvalid, "%v", err)
	}
	now := config.CloneTasks(d.cfg.Tasks)
	d.cfgMu.Unlock()
	d.log.Info("task sources changed", "op", opName(req))

	d.tasks.reconfigure(now)
	note := ""
	if sync != "" {
		if i := slices.IndexFunc(d.tasks.list(), func(ts *taskSource) bool { return ts.conf().SourceName() == sync }); i >= 0 {
			note = d.tasks.syncQueue(ctx, d.tasks.list()[i])
		}
	}
	return &fleetv1.EditTasksResponse{Sources: d.tasks.listSources(), Note: note}, nil
}

func opName(req *fleetv1.EditTasksRequest) string {
	switch req.GetOp().(type) {
	case *fleetv1.EditTasksRequest_AddSource:
		return "add"
	case *fleetv1.EditTasksRequest_RemoveSource:
		return "remove"
	case *fleetv1.EditTasksRequest_MapProject:
		return "map"
	case *fleetv1.EditTasksRequest_UnmapProject:
		return "unmap"
	case *fleetv1.EditTasksRequest_SetOption:
		return "set"
	}
	return "?"
}

// editTasksLocked applies req to d.cfg (cfgMu held) and returns the source
// whose queue should follow its mapped projects, "" for none.
func (d *daemon) editTasksLocked(req *fleetv1.EditTasksRequest) (string, error) {
	c := d.cfg
	sourceName := func(name string) string {
		if i, err := c.TaskSourceIndex(name); err == nil {
			return c.Tasks[i].SourceName()
		}
		return name
	}
	switch op := req.GetOp().(type) {
	case *fleetv1.EditTasksRequest_AddSource:
		s := op.AddSource
		if s.GetTokenFile() == "" && s.GetTokenEnv() == "" {
			return "", errf(codeInvalid, "a token is required: a token file, or an environment variable of the daemon")
		}
		ts := config.TaskSource{
			Name: strings.TrimSpace(s.GetName()), Kind: strings.TrimSpace(s.GetKind()), URL: strings.TrimSpace(s.GetUrl()),
			TokenFile: strings.TrimSpace(s.GetTokenFile()), TokenEnv: strings.TrimSpace(s.GetTokenEnv()), Webhook: strings.TrimSpace(s.GetWebhook()),
		}
		if ts.Name == ts.Kind || ts.Name == "rome" && ts.Kind == "" {
			ts.Name = "" // the default
		}
		if _, err := ts.Secret(); err != nil {
			return "", errf(codeInvalid, "%v", err)
		}
		return "", c.AddTaskSource(ts)

	case *fleetv1.EditTasksRequest_RemoveSource:
		return "", c.RemoveTaskSource(op.RemoveSource)

	case *fleetv1.EditTasksRequest_MapProject:
		m := op.MapProject
		key := strings.ToUpper(strings.TrimSpace(m.GetKey()))
		if key == "" {
			return "", errf(codeInvalid, "a project key is required")
		}
		p := config.TaskProjectPatch{
			Path: m.Path, Root: m.Root, Adapter: m.Adapter, Model: m.Model, Effort: m.Effort,
			Isolation: m.Isolation, Sandbox: m.Sandbox, Instructions: m.Instructions,
		}
		if m.GetSetArgs() {
			args := m.GetArgs()
			p.Args = &args
		}
		proj, _, err := c.MapTaskProject(m.GetSource(), key, p)
		if err != nil {
			return "", err
		}
		name := sourceName(m.GetSource())
		if err := d.checkTaskProject(c, name, proj); err != nil {
			return "", err
		}
		return name, nil

	case *fleetv1.EditTasksRequest_UnmapProject:
		name := sourceName(op.UnmapProject.GetSource())
		return name, c.UnmapTaskProject(op.UnmapProject.GetSource(), strings.TrimSpace(op.UnmapProject.GetKey()))

	case *fleetv1.EditTasksRequest_SetOption:
		o := op.SetOption
		key := strings.ToLower(strings.TrimSpace(o.GetKey()))
		if err := c.SetTaskOption(o.GetSource(), key, o.GetValues()); err != nil {
			return "", err
		}
		i, _ := c.TaskSourceIndex(o.GetSource())
		s := c.Tasks[i]
		switch key {
		case "token_file", "token_env":
			if _, err := s.Secret(); err != nil {
				return "", errf(codeInvalid, "%v", err)
			}
		case "adapter":
			if s.Adapter != "" {
				if _, ok := d.opts.Adapters.Get(s.Adapter); !ok {
					return "", errf(codeNotFound, "unknown adapter %q", s.Adapter)
				}
			}
		case "webhook":
			return s.SourceName(), nil
		}
		return "", nil
	}
	return "", errf(codeInvalid, "add_source, remove_source, map_project, unmap_project or set_option is required")
}

// checkTaskProject fails unless project p of source can start agents: its
// folder is inside a root and its adapter is known.
func (d *daemon) checkTaskProject(c *config.Config, source string, p config.TaskProject) error {
	root, rel, abs := p.ProjectDir()
	if _, _, err := c.Resolve(root, rel, abs); err != nil {
		return errf(codeInvalid, "project %s: %v (add the folder as a root with `fleet roots add`)", p.Key, resolveErr(err))
	}
	i, err := c.TaskSourceIndex(source)
	if err != nil {
		return err
	}
	if a := c.Tasks[i].Agent(p).Adapter; a != "" {
		if _, ok := d.opts.Adapters.Get(a); !ok {
			return errf(codeNotFound, "unknown adapter %q", a)
		}
	}
	return nil
}
