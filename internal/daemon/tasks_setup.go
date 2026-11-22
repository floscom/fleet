package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/tasks"
)

// Setup on the tracker (tasks.Setuper, Rome's agent setup): fleet asks the
// tracker's people for the folder of each project the account is a member
// of, works a project set up there in that folder unless config.toml maps
// it, and reports per project whether it can work it. A project that
// becomes ready gets the tickets assigned to the account before (catchUp).

// folderSetting is the setting fleet asks for.
var folderSetting = tasks.SetupField{
	Key:   "folder",
	Label: "Folder",
	Help: "Absolute path of the project's checkout on the machine fleet runs on, inside one of its root folders (fleet roots). " +
		"fleet starts the agents for the project's tickets there. A mapping in fleet's config.toml wins.",
}

// syncSetups reads the setup of each source whose tracker has one when it
// is due: every taskSetupSync, and at once after a change.
func (t *taskManager) syncSetups(ctx context.Context) {
	now := time.Now()
	for _, ts := range t.list() {
		su, ok := ts.src.(tasks.Setuper)
		if !ok {
			continue
		}
		t.mu.Lock()
		due := ts.me != nil && now.After(ts.setupOff) && (ts.setupDue || now.Sub(ts.setupAt) >= taskSetupSync)
		if due {
			ts.setupDue, ts.setupAt = false, now
		}
		t.mu.Unlock()
		if due {
			t.syncSetup(ctx, ts, su)
		}
	}
}

// syncSetup declares fleet's settings (once per start of the source),
// reads the setup, reports each project whose state changed, and takes up
// the tickets of the projects that became ready.
func (t *taskManager) syncSetup(ctx context.Context, ts *taskSource, su tasks.Setuper) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	name := ts.conf().SourceName()
	log := t.d.log.With("source", name)
	t.mu.Lock()
	declared := ts.declared
	t.mu.Unlock()
	if !declared {
		if err := su.DeclareSetup(ctx, []tasks.SetupField{folderSetting}); err != nil {
			t.setupFailed(ts, err)
			return
		}
		t.mu.Lock()
		ts.declared = true
		t.mu.Unlock()
		log.Info("asked the task tracker for the folder of each project")
	}
	ps, err := su.ProjectSetups(ctx)
	if err != nil {
		t.setupFailed(ts, err)
		return
	}
	cfg := t.d.config()
	machine := machineName(cfg)
	dirs := map[string]string{}
	var views []*fleetv1.TaskProject
	var ready []string
	for _, p := range ps {
		j := judgeSetup(&cfg, ts.conf(), p, machine)
		if j.tracker {
			v := &fleetv1.TaskProject{Key: p.Project, Path: j.dir, Adapter: ts.conf().Agent(config.TaskProject{}).Adapter, Error: j.problem, Tracker: true}
			views = append(views, v)
			if j.report.State == tasks.SetupReady {
				dirs[p.Project] = j.dir
			}
		}
		if p.State == j.report.State && p.Report.Message == j.report.Message {
			continue
		}
		if err := su.ReportSetup(ctx, p.Project, j.report); err != nil {
			log.Warn("could not report a project's setup to the task tracker", "project", p.Project, "err", err)
			continue
		}
		log.Info("reported a project's setup to the task tracker", "project", p.Project, "state", j.report.State, "message", j.report.Message)
		if j.report.State == tasks.SetupReady && p.State != tasks.SetupReady {
			ready = append(ready, p.Project)
		}
	}
	t.mu.Lock()
	if ts.setupErr != "" {
		log.Info("the task tracker's setup can be read again")
	}
	changed := !slices.Equal(slices.Sorted(maps.Keys(ts.setupDirs)), slices.Sorted(maps.Keys(dirs)))
	ts.setupDirs, ts.setupViews, ts.setupErr = dirs, views, ""
	t.mu.Unlock()
	if changed {
		if note := t.syncQueue(ctx, ts); note != "" {
			log.Info("brought the task queue in line with the projects set up", "note", note)
		}
	}
	for _, key := range ready {
		t.catchUp(ctx, ts, su, key)
	}
}

// setupFailed records a failed setup call: a tracker without setup (or a
// token that may not use it) is asked again after taskSetupRetry, other
// errors on the next sync.
func (t *taskManager) setupFailed(ts *taskSource, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	log := t.d.log.With("source", ts.conf().SourceName())
	if errors.Is(err, tasks.ErrUnsupported) || errors.Is(err, tasks.ErrAuth) {
		ts.setupOff = time.Now().Add(taskSetupRetry)
		if ts.setupErr != err.Error() {
			log.Info("the task tracker has no setup fleet can use; projects need a mapping in config.toml", "err", err, "retry_in", taskSetupRetry)
		}
	} else if ts.setupErr != err.Error() {
		log.Warn("reading the task tracker's setup failed", "err", err)
	}
	ts.setupErr = err.Error()
}

// setupJudgment is what fleet makes of a project's setup.
type setupJudgment struct {
	// dir is the folder it works the project in, when it can.
	dir string
	// tracker: the folder is the tracker's to give (config.toml does not
	// map the project).
	tracker bool
	report  tasks.SetupReport
	// problem is why fleet cannot work it, short, for `fleet tasks`.
	problem string
}

// judgeSetup decides what fleet makes of the setup of project p: a mapping
// in config.toml wins over the folder people filled in on the tracker.
func judgeSetup(cfg *config.Config, c config.TaskSource, p tasks.ProjectSetup, machine string) setupJudgment {
	if proj, ok := c.Project(p.Project); ok {
		root, rel, abs := proj.ProjectDir()
		if _, dir, err := cfg.Resolve(root, rel, abs); err != nil {
			return setupJudgment{report: tasks.SetupReport{State: tasks.SetupBlocked,
				Message: fmt.Sprintf("fleet on %s maps %s to a folder in its config.toml that it cannot use: %v", machine, p.Project, err)}}
		} else {
			return setupJudgment{dir: dir, report: tasks.SetupReport{State: tasks.SetupReady,
				Message: fmt.Sprintf("fleet on %s works it in %s (set in its config.toml).", machine, dir)}}
		}
	}
	folder := strings.TrimSpace(p.Values[folderSetting.Key])
	if folder == "" {
		return setupJudgment{tracker: true, problem: "no folder set up in " + c.Kind + " yet", report: tasks.SetupReport{State: tasks.SetupBlocked,
			Message: fmt.Sprintf("Fill in Folder: the absolute path of the project's checkout on %s, inside one of fleet's root folders.", machine)}}
	}
	dir, err := setupDir(cfg, folder)
	if err != nil {
		return setupJudgment{tracker: true, problem: fmt.Sprintf("%s: %v", folder, err), report: tasks.SetupReport{State: tasks.SetupBlocked,
			Message: fmt.Sprintf("fleet on %s cannot use the folder %s: %v.", machine, folder, err)}}
	}
	return setupJudgment{dir: dir, tracker: true, report: tasks.SetupReport{State: tasks.SetupReady,
		Message: fmt.Sprintf("fleet on %s works it in %s.", machine, dir)}}
}

// setupDir resolves a folder people filled in: absolute, and inside a root.
func setupDir(cfg *config.Config, folder string) (string, error) {
	if !filepath.IsAbs(folder) {
		return "", errors.New("it is not an absolute path")
	}
	_, dir, err := cfg.Resolve("", "", folder)
	return dir, err
}

// setupDirLocked is the folder of project, set up on the tracker, that
// fleet works it in, "" if none: the one message m carries (rendered when
// it was sent), else the one last read.
func setupDirLocked(cfg *config.Config, ts *taskSource, m tasks.Message, project string) string {
	key := strings.ToUpper(project)
	vals, ok := m.Setup[key]
	if !ok {
		return ts.setupDirs[key]
	}
	folder := strings.TrimSpace(vals[folderSetting.Key])
	dir := ""
	if folder != "" {
		if d, err := setupDir(cfg, folder); err == nil {
			dir = d
		}
	}
	if dir != ts.setupDirs[key] {
		ts.setupDue = true // newer than what fleet reported
	}
	return dir
}

// catchUp takes up the tickets assigned to the account in project key that
// nobody started: they came while fleet could not work the project, and
// were let go. They wait for an agent like received ones.
func (t *taskManager) catchUp(ctx context.Context, ts *taskSource, su tasks.Setuper, key string) {
	name := ts.conf().SourceName()
	keys, err := su.Unstarted(ctx, key)
	if err != nil {
		t.d.log.Warn("could not list the tickets assigned before a project was set up", "source", name, "project", key, "err", err)
		return
	}
	t.mu.Lock()
	handle := "the account"
	if ts.me != nil {
		handle = "@" + ts.me.Handle
	}
	t.mu.Unlock()
	now := time.Now()
	m := tasks.Message{ID: fmt.Sprintf("setup-%s-%d", key, now.UnixMilli())}
	var took []string
	for _, k := range keys {
		if t.d.agents.taskAgent(name, k) != "" {
			continue
		}
		took = append(took, k)
		m.Events = append(m.Events, tasks.Event{
			ID: m.ID + "-" + k, Kind: tasks.Assigned, Project: key, Issue: k, At: now,
			Text: fmt.Sprintf("The ticket was assigned to you (%s) before fleet could work %s; now it can.", handle, key),
		})
	}
	if len(took) == 0 {
		return
	}
	t.d.log.Info("taking up the tickets assigned before their project was set up", "source", name, "project", key, "tickets", strings.Join(took, " "))
	t.take(ctx, ts, m)
}

// queueKeys are the projects ts's queue should carry: the ones config.toml
// maps and the ones set up on the tracker that fleet can work.
func (t *taskManager) queueKeys(ts *taskSource) []string {
	keys := mappedKeys(ts.conf())
	t.mu.Lock()
	for k := range ts.setupDirs {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	t.mu.Unlock()
	slices.Sort(keys)
	return keys
}

// machineName is how reports name this machine: its fleet name, else its
// host name.
func machineName(cfg config.Config) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "this machine"
}
