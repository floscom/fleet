package daemon

// Task trackers ([[tasks]] in config.toml, package tasks). Each source's
// queue (Rome: its account's pull webhook) is received from a message at a
// time, and only while fewer than max_agents of its agents are busy and no
// message received earlier still waits for an agent: the tracker holds
// the backlog, and its collected delivery batches it.
//
// A message is a batch of events. News on a ticket an agent works (an
// answer, a mention, a decision) is queued for that agent. The other
// tickets, assigned to the account or needing it again, are grouped by
// project: one agent per group works its tickets in order, in the folder
// the project is mapped to (or the batch's route folder), told how to work
// the tracker and given its MCP server and token. The message stays leased
// while its agents work and is acknowledged once they are done with their
// turn. A group that cannot start leaves its message to the tracker: the
// lease runs out and the message is handed out again (dead after the
// tracker's max_receive_count, which tells a person). Events are
// deduplicated by ID, so a message handed out again repeats nothing done.
// While its agents work, the daemon sends the tracker the account's
// heartbeats.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/tasks"
	"fleet/internal/tasks/rome"
)

// Variables for tests.
var (
	// taskTick is how often leases, waiting groups, finished work and
	// presence are looked at.
	taskTick = 5 * time.Second
	// taskBeat is how often a working account's heartbeat is repeated
	// (Rome shows it offline after 180 s, asks for one every 60 s).
	taskBeat = 45 * time.Second
	// taskLease is the lease asked for on a message, at receive and at
	// each renewal; taskRenew renews it when less is left.
	taskLease = 15 * time.Minute
	taskRenew = 5 * time.Minute
	// taskWait is a receive's long-poll; taskPollMin the least time
	// between two receives that bring nothing.
	taskWait    = 20 * time.Second
	taskPollMin = 2 * time.Second
	// taskIdleDone: an agent never seen busy counts as done once it idled
	// this long after its start.
	taskIdleDone = 2 * time.Minute
	// taskCloseCheck is how often the tickets of an agent idle for its
	// source's close_after are looked up (closeDone).
	taskCloseCheck = 5 * time.Minute
	// taskRetry is the first wait after a failed receive; it doubles up
	// to taskRetryMax. A refused token or a missing queue waits
	// taskAuthRetry (`fleet tasks setup` ends that wait).
	taskRetry     = 5 * time.Second
	taskRetryMax  = 5 * time.Minute
	taskAuthRetry = 10 * time.Minute
	// newTaskSource builds the source of a [[tasks]] entry.
	newTaskSource = func(c config.TaskSource, token string) (tasks.Source, error) {
		switch c.Kind {
		case rome.Kind:
			return rome.New(c.SourceName(), c.URL, token, c.Webhook), nil
		}
		return nil, fmt.Errorf("unknown task source kind %q", c.Kind)
	}
)

const (
	// maxSeen is how many handled event IDs a source remembers.
	maxSeen = 2000
	// maxInFlight caps the messages a source holds leased.
	maxInFlight = 20
	// maxStartTries is how often starting a group is tried.
	maxStartTries = 3
)

// agentTask is agentRec.Task.
type agentTask struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	// Key is the first ticket, Keys all of them (when more than one), in
	// the order worked.
	Key   string   `json:"key"`
	Keys  []string `json:"keys,omitempty"`
	Title string   `json:"title,omitempty"`
	URL   string   `json:"url,omitempty"`
}

func (t *agentTask) proto() *fleetv1.AgentTask {
	if t == nil {
		return nil
	}
	return &fleetv1.AgentTask{Source: t.Source, Kind: t.Kind, Key: t.Key, Keys: t.keys(), Title: t.Title, Url: t.URL}
}

func (t *agentTask) keys() []string {
	if len(t.Keys) > 0 {
		return t.Keys
	}
	return []string{t.Key}
}

// works reports whether t covers ticket key of source.
func (t *agentTask) works(source, key string) bool {
	return t != nil && t.Source == source && slices.ContainsFunc(t.keys(), func(k string) bool { return strings.EqualFold(k, key) })
}

// taskState is tasks.json.
type taskState struct {
	Sources map[string]*sourceState `json:"sources"`
	// Idle is since when each live agent started for tickets has been
	// idle (Unix ms), by agent ID (closeDone).
	Idle map[string]int64 `json:"idle,omitempty"`
}

// sourceState is where a source stands.
type sourceState struct {
	// Seen are the latest handled event IDs, oldest first.
	Seen []string `json:"seen,omitempty"`
	// InFlight are the messages received and not yet done, oldest first.
	InFlight []*leasedMsg `json:"in_flight,omitempty"`
}

// leasedMsg is a received message whose work is going on.
type leasedMsg struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	// LeaseMs is when its lease ends (Unix ms).
	LeaseMs      int64             `json:"lease_ms"`
	ReceivedMs   int64             `json:"received_ms"`
	ReceiveCount int               `json:"receive_count,omitempty"`
	Prompt       string            `json:"prompt,omitempty"`
	Route        map[string]string `json:"route,omitempty"`
	Groups       []*ticketGroup    `json:"groups"`
}

// ticketGroup are the tickets of a message one agent works.
type ticketGroup struct {
	// Project is the tickets' project; Folder the route folder they go to
	// when the project is mapped to none.
	Project string `json:"project"`
	Folder  string `json:"folder,omitempty"`
	// Events are one per ticket, in order; EventIDs all the events merged
	// into them.
	Events   []tasks.Event `json:"events"`
	EventIDs []string      `json:"event_ids,omitempty"`
	// Started once its agent started (Agent, StartedMs), or it needed
	// none; Failed when it could not start.
	Started   bool   `json:"started,omitempty"`
	Agent     string `json:"agent,omitempty"`
	StartedMs int64  `json:"started_ms,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
	Tries     int    `json:"tries,omitempty"`
	// Busy once its agent was seen at work; Done once it was done.
	Busy bool `json:"busy,omitempty"`
	Done bool `json:"done,omitempty"`
}

func (g *ticketGroup) keys() []string {
	out := make([]string, len(g.Events))
	for i, e := range g.Events {
		out[i] = e.Issue
	}
	return out
}

func (g *ticketGroup) pending() bool { return !g.Started && !g.Failed }

// taskSource is a configured source, while the daemon runs.
type taskSource struct {
	// cfg is its [[tasks]] entry, replaced whole when it is edited
	// (taskManager.reconfigure).
	cfg  atomic.Pointer[config.TaskSource]
	src  tasks.Source // nil when it cannot run (no token)
	kick chan struct{}
	// stop ends its receive loop, which closes done.
	stop context.CancelFunc
	done chan struct{}
	// Guarded by taskManager.mu:
	me       *tasks.Account
	queue    string
	err      string
	lastPoll time.Time
	beat     tasks.Presence
	beatAt   time.Time
}

// conf returns its [[tasks]] entry as it is now.
func (ts *taskSource) conf() config.TaskSource { return *ts.cfg.Load() }

// taskManager runs the task sources.
type taskManager struct {
	d    *daemon
	path string
	// wg counts the receive loops.
	wg sync.WaitGroup

	mu sync.Mutex
	// ctx is run's, nil before it runs.
	ctx     context.Context
	sources []*taskSource
	state   taskState
	// checked is when closeDone last looked up an agent's tickets.
	checked map[string]time.Time
	// startMu serializes starting agents for tickets.
	startMu sync.Mutex
}

// newTaskManager sets up the sources of cfg.
func newTaskManager(d *daemon, cfg config.Config, path string) *taskManager {
	t := &taskManager{d: d, path: path, checked: map[string]time.Time{}}
	t.load()
	for _, c := range cfg.Tasks {
		t.sources = append(t.sources, t.newSourceLocked(c))
	}
	return t
}

// newSourceLocked sets up the source of c, not running yet.
func (t *taskManager) newSourceLocked(c config.TaskSource) *taskSource {
	ts := &taskSource{kick: make(chan struct{}, 1)}
	ts.cfg.Store(&c)
	switch tok, err := c.Secret(); {
	case c.Disabled:
	case err != nil:
		ts.err = err.Error()
	default:
		if ts.src, err = newTaskSource(c, tok); err != nil {
			ts.err = err.Error()
		}
	}
	if ts.err != "" {
		t.d.log.Warn("task source cannot run", "source", c.SourceName(), "err", ts.err)
	}
	if t.state.Sources[c.SourceName()] == nil {
		t.state.Sources[c.SourceName()] = &sourceState{}
	}
	return ts
}

// list returns the sources as they are now.
func (t *taskManager) list() []*taskSource {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.sources)
}

// startSource starts ts's receive loop, under run's context.
func (t *taskManager) startSource(ctx context.Context, ts *taskSource) {
	if ts.src == nil {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	ts.stop, ts.done = cancel, make(chan struct{})
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(ts.done)
		t.receive(sctx, ts)
	}()
}

// stopSource ends ts's receive loop and waits for it. Its messages in
// flight stay leased until the lease runs out (or the source, restarted,
// renews them).
func (t *taskManager) stopSource(ts *taskSource) {
	if ts.stop != nil {
		ts.stop()
		<-ts.done
	}
}

// reconfigure applies edited [[tasks]] entries: a source whose connection
// changed (or a new one) starts afresh, the others take their new settings
// as they run, removed ones stop.
func (t *taskManager) reconfigure(entries []config.TaskSource) {
	t.mu.Lock()
	old := t.sources
	var next, stop, start []*taskSource
	for _, c := range entries {
		i := slices.IndexFunc(old, func(ts *taskSource) bool { return ts.conf().SourceName() == c.SourceName() })
		if i >= 0 && config.SameConnection(old[i].conf(), c) {
			old[i].cfg.Store(&c)
			next = append(next, old[i])
			continue
		}
		if i >= 0 {
			stop = append(stop, old[i])
		}
		ts := t.newSourceLocked(c)
		next = append(next, ts)
		start = append(start, ts)
	}
	for _, ts := range old {
		if !slices.Contains(next, ts) && !slices.Contains(stop, ts) {
			stop = append(stop, ts)
		}
	}
	t.sources = next
	ctx := t.ctx
	t.mu.Unlock()
	for _, ts := range stop {
		t.stopSource(ts)
		t.d.log.Info("task source stopped", "source", ts.conf().SourceName())
	}
	if ctx == nil {
		return // run starts them
	}
	for _, ts := range start {
		t.startSource(ctx, ts)
		if ts.src != nil {
			t.d.log.Info("task source started", "source", ts.conf().SourceName())
		}
	}
}

func (t *taskManager) load() {
	t.state = taskState{Sources: map[string]*sourceState{}, Idle: map[string]int64{}}
	b, err := os.ReadFile(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(b, &t.state)
	}
	if err != nil {
		t.d.log.Warn("tasks.json unreadable; starting over", "err", err)
		t.state = taskState{}
	}
	if t.state.Sources == nil {
		t.state.Sources = map[string]*sourceState{}
	}
	if t.state.Idle == nil {
		t.state.Idle = map[string]int64{}
	}
	for name, st := range t.state.Sources {
		if st == nil {
			t.state.Sources[name] = &sourceState{}
		}
	}
}

func (t *taskManager) saveLocked() {
	b, err := json.MarshalIndent(t.state, "", "  ")
	if err == nil {
		err = writeFileAtomic(t.path, b)
	}
	if err != nil {
		t.d.log.Warn("saving tasks.json failed", "err", err)
	}
}

func seenLocked(st *sourceState, id string) bool {
	return id != "" && slices.Contains(st.Seen, id)
}

func markSeenLocked(st *sourceState, ids ...string) {
	for _, id := range ids {
		if id != "" && !slices.Contains(st.Seen, id) {
			st.Seen = append(st.Seen, id)
		}
	}
	if len(st.Seen) > maxSeen {
		st.Seen = slices.Delete(st.Seen, 0, len(st.Seen)-maxSeen)
	}
}

// run receives from every source until ctx ends, and looks after the work
// received every taskTick.
func (t *taskManager) run(ctx context.Context) {
	t.mu.Lock()
	t.ctx = ctx
	for _, ts := range t.sources {
		t.startSource(ctx, ts)
	}
	t.mu.Unlock()
	tick := time.NewTicker(taskTick)
	defer tick.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-tick.C:
			t.renew(ctx)
			t.startPending(ctx)
			t.finish(ctx)
			t.heartbeats(ctx)
			t.closeDone(ctx)
		}
	}
	t.wg.Wait()
	t.goIdle()
}

// canReceive reports whether ts may take another message: an agent to
// spare, and every message it holds has its agents started.
func (t *taskManager) canReceive(ts *taskSource) bool {
	t.mu.Lock()
	st := t.state.Sources[ts.conf().SourceName()]
	waiting := slices.ContainsFunc(st.InFlight, func(l *leasedMsg) bool { return slices.ContainsFunc(l.Groups, (*ticketGroup).pending) })
	full := len(st.InFlight) >= maxInFlight
	t.mu.Unlock()
	return !waiting && !full && t.d.agents.taskWorking(ts.conf().SourceName()) < ts.conf().MaxAgentsOrDefault()
}

// receive takes messages of ts's queue, one at a time, until ctx ends.
func (t *taskManager) receive(ctx context.Context, ts *taskSource) {
	name := ts.conf().SourceName()
	wait := taskRetry
	for ctx.Err() == nil {
		if !t.canReceive(ts) {
			t.sleep(ctx, ts, taskTick)
			continue
		}
		t.mu.Lock()
		known := ts.me != nil
		t.mu.Unlock()
		if !known {
			// Show who the source acts as before the first long-poll ends.
			if me, err := ts.src.Me(ctx); err == nil {
				t.mu.Lock()
				ts.me = &me
				t.mu.Unlock()
			}
		}
		began := time.Now()
		msgs, err := ts.src.Receive(ctx, 1, taskWait, taskLease)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			t.mu.Lock()
			changed := ts.err != err.Error()
			ts.err = err.Error()
			t.mu.Unlock()
			d := wait
			var re *tasks.RetryError
			switch {
			case errors.Is(err, tasks.ErrAuth), errors.Is(err, tasks.ErrNoQueue):
				d = taskAuthRetry
			case errors.As(err, &re):
				d = re.After
			default:
				wait = min(wait*2, taskRetryMax)
			}
			if changed {
				t.d.log.Warn("task source receive failed", "source", name, "err", err, "retry_in", d)
			}
			t.sleep(ctx, ts, d)
			continue
		}
		wait = taskRetry
		me, merr := ts.src.Me(ctx)
		queue, _ := ts.src.Queue(ctx)
		t.mu.Lock()
		if ts.err != "" {
			t.d.log.Info("task source receives again", "source", name)
		}
		ts.err, ts.lastPoll, ts.queue = "", time.Now(), queue
		if merr == nil {
			ts.me = &me
		}
		t.mu.Unlock()
		for _, m := range msgs {
			t.take(ctx, ts, m)
		}
		if len(msgs) > 0 {
			t.startPending(ctx)
		} else if time.Since(began) < taskPollMin {
			// The tracker answered at once with nothing: don't spin.
			t.sleep(ctx, ts, taskPollMin)
		}
	}
}

// sleep waits d, or until ts is kicked or ctx ends.
func (t *taskManager) sleep(ctx context.Context, ts *taskSource, d time.Duration) {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
	case <-tm.C:
	case <-ts.kick:
	}
}

// take sorts a received message: news goes to the agents working its
// tickets now, the other tickets into groups waiting for an agent. A
// message with nothing left to do is acknowledged at once.
func (t *taskManager) take(ctx context.Context, ts *taskSource, m tasks.Message) {
	name := ts.conf().SourceName()
	log := t.d.log.With("source", name, "message", m.ID)
	t.mu.Lock()
	st := t.state.Sources[name]
	if i := slices.IndexFunc(st.InFlight, func(l *leasedMsg) bool { return l.ID == m.ID }); i >= 0 {
		// Handed out again while fleet still works it (its lease ran out):
		// go on with it under the new handle.
		l := st.InFlight[i]
		l.Handle, l.LeaseMs, l.ReceiveCount = m.Handle, time.Now().Add(taskLease).UnixMilli(), m.ReceiveCount
		t.saveLocked()
		t.mu.Unlock()
		return
	}
	now := time.Now()
	l := &leasedMsg{
		ID: m.ID, Handle: m.Handle, LeaseMs: now.Add(taskLease).UnixMilli(), ReceivedMs: now.UnixMilli(),
		ReceiveCount: m.ReceiveCount, Prompt: m.Prompt, Route: m.Route,
	}
	folder := strings.TrimSpace(m.Route["folder"])
	for _, ev := range m.Events {
		if seenLocked(st, ev.ID) {
			continue
		}
		elog := log.With("issue", ev.Issue)
		_, mapped := ts.conf().Project(ev.Project)
		if !mapped && folder == "" {
			elog.Info("task in a project mapped to no folder; ignored", "project", ev.Project)
			markSeenLocked(st, ev.ID)
			continue
		}
		if id := t.d.agents.taskAgent(name, ev.Issue); id != "" {
			if err := t.d.agents.queueTaskNews(id, tasks.NewsMessage(ts.src, ev)); err != nil {
				elog.Warn("could not queue task news for its agent", "agent", id, "err", err)
			} else {
				elog.Info("queued task news for its agent", "agent", id)
			}
			markSeenLocked(st, ev.ID)
			continue
		}
		// Waiting in a group already, of this message or an earlier one.
		if g, i := pendingTicketLocked(st, l, ev.Issue); g != nil {
			g.Events[i].Text = strings.TrimSpace(g.Events[i].Text + "\n\n" + ev.Text)
			g.EventIDs = append(g.EventIDs, ev.ID)
			continue
		}
		if ev.Kind != tasks.Assigned && !ev.Start {
			elog.Info("task news for a ticket no agent works; ignored")
			markSeenLocked(st, ev.ID)
			continue
		}
		g := groupFor(l, ev.Project, mapped, folder)
		g.Events = append(g.Events, ev)
		g.EventIDs = append(g.EventIDs, ev.ID)
	}
	if len(l.Groups) == 0 {
		t.saveLocked()
		t.mu.Unlock()
		if err := ts.src.Ack(ctx, m.Handle); err != nil && !errors.Is(err, tasks.ErrGone) {
			log.Warn("acknowledging a task message failed", "err", err)
		}
		return
	}
	st.InFlight = append(st.InFlight, l)
	t.saveLocked()
	for _, g := range l.Groups {
		log.Info("tasks waiting for an agent", "tickets", strings.Join(g.keys(), " "))
	}
	t.mu.Unlock()
}

// groupFor returns l's group for tickets of project (its folder when
// mapped, else the route folder), adding it.
func groupFor(l *leasedMsg, project string, mapped bool, folder string) *ticketGroup {
	if mapped {
		folder = ""
	}
	for _, g := range l.Groups {
		if g.Folder == folder && (folder != "" || strings.EqualFold(g.Project, project)) {
			return g
		}
	}
	g := &ticketGroup{Project: project, Folder: folder}
	l.Groups = append(l.Groups, g)
	return g
}

// pendingTicketLocked finds ticket key in a group waiting to start, of
// the messages in flight or cur.
func pendingTicketLocked(st *sourceState, cur *leasedMsg, key string) (*ticketGroup, int) {
	for _, l := range append(slices.Clone(st.InFlight), cur) {
		for _, g := range l.Groups {
			if !g.pending() {
				continue
			}
			for i, e := range g.Events {
				if strings.EqualFold(e.Issue, key) {
					return g, i
				}
			}
		}
	}
	return nil, 0
}

// startPending starts waiting groups, oldest first, while their sources
// have agents to spare.
func (t *taskManager) startPending(ctx context.Context) {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	for _, ts := range t.list() {
		if ts.src == nil {
			continue
		}
		name := ts.conf().SourceName()
		for ctx.Err() == nil {
			t.mu.Lock()
			var l *leasedMsg
			var g *ticketGroup
			for _, m := range t.state.Sources[name].InFlight {
				if i := slices.IndexFunc(m.Groups, (*ticketGroup).pending); i >= 0 {
					l, g = m, m.Groups[i]
					break
				}
			}
			t.mu.Unlock()
			if g == nil || t.d.agents.taskWorking(name) >= ts.conf().MaxAgentsOrDefault() {
				break
			}
			err := t.startGroup(ctx, ts, l, g)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			t.mu.Lock()
			g.Tries++
			log := t.d.log.With("source", name, "message", l.ID, "tickets", strings.Join(g.keys(), " "), "tries", g.Tries)
			if g.Tries >= maxStartTries {
				g.Failed = true
				log.Warn("could not start an agent for tasks; leaving the message for the tracker to hand out again", "err", err)
			} else {
				log.Warn("could not start an agent for tasks; will try again", "err", err)
			}
			t.saveLocked()
			t.mu.Unlock()
			break // try again on the next tick
		}
	}
}

// startGroup starts the agent of group g of message l.
func (t *taskManager) startGroup(ctx context.Context, ts *taskSource, l *leasedMsg, g *ticketGroup) error {
	name := ts.conf().SourceName()
	t.mu.Lock()
	evs, ids, folder, routed := slices.Clone(g.Events), slices.Clone(g.EventIDs), g.Folder, l.Prompt
	t.mu.Unlock()
	// A ticket an agent took meanwhile (fleet tasks start) gets the news.
	var rest []tasks.Event
	for _, ev := range evs {
		if id := t.d.agents.taskAgent(name, ev.Issue); id != "" {
			if err := t.d.agents.queueTaskNews(id, tasks.NewsMessage(ts.src, ev)); err != nil {
				t.d.log.Warn("could not queue task news for its agent", "source", name, "issue", ev.Issue, "agent", id, "err", err)
			}
			continue
		}
		rest = append(rest, ev)
	}
	var a *fleetv1.Agent
	if len(rest) > 0 {
		var err error
		if a, err = t.startTickets(ctx, ts, rest, folder, routed); err != nil {
			return err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	g.Started, g.StartedMs = true, time.Now().UnixMilli()
	if a != nil {
		g.Agent = a.GetId()
	} else {
		g.Done = true
	}
	markSeenLocked(t.state.Sources[name], ids...)
	t.saveLocked()
	return nil
}

var errTaskNotMapped = errors.New("project mapped to no folder")

// startTickets starts an agent on tickets evs (of one project), in the
// project's folder or, for a project mapped to none, in folder.
func (t *taskManager) startTickets(ctx context.Context, ts *taskSource, evs []tasks.Event, folder, routed string) (*fleetv1.Agent, error) {
	name := ts.conf().SourceName()
	proj, mapped := ts.conf().Project(evs[0].Project)
	if !mapped && folder == "" {
		return nil, fmt.Errorf("%s: %w", evs[0].Project, errTaskNotMapped)
	}
	me, err := ts.src.Me(ctx)
	if err != nil {
		return nil, err
	}
	tickets := make([]tasks.Ticket, 0, len(evs))
	keys := make([]string, 0, len(evs))
	for _, ev := range evs {
		issue, err := ts.src.Issue(ctx, ev.Issue)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ev.Issue, err)
		}
		tickets = append(tickets, tasks.Ticket{Issue: issue, Event: ev})
		keys = append(keys, issue.Key)
	}
	setup := ts.src.Setup(me)
	cfg := t.d.config()
	ac := ts.conf().Agent(proj)
	req := &fleetv1.RunAgentRequest{
		Adapter:   ac.Adapter,
		Name:      tasks.AgentName(keys),
		Prompt:    tasks.Prompt(ts.src, tickets),
		Model:     ac.Model,
		Effort:    ac.Effort,
		ExtraArgs: ac.Args,
	}
	if mapped {
		req.Root, req.Path, req.AbsolutePath = proj.ProjectDir()
	} else if req.AbsolutePath = folder; !filepath.IsAbs(folder) {
		return nil, fmt.Errorf("route folder %q is not an absolute path", folder)
	}
	switch ac.Isolation {
	case "worktree":
		req.Isolation = isoWorktree
	case "pinned":
		req.Isolation = isoPinned
	}
	switch ac.Sandbox {
	case "none":
		req.Sandbox = sandboxNone
	case "docker":
		req.Sandbox = sandboxDocker
	}
	first := tickets[0].Issue
	task := &agentTask{Source: name, Kind: ts.conf().Kind, Key: first.Key, Title: first.Title, URL: first.URL}
	if len(keys) > 1 {
		task.Keys = keys
	}
	x := launchExtra{
		task:         task,
		env:          setup.Env,
		instructions: tasks.Instructions(ts.src, setup, cfg.Name, routed, ac.Instructions),
		mcp:          setup.MCPServers,
	}
	a, err := t.d.agents.runWith(ctx, req, x)
	if isCode(err, codeExists) {
		// Another live agent has the tickets' name: let the daemon pick one.
		req.Name = ""
		a, err = t.d.agents.runWith(ctx, req, x)
	}
	if err != nil {
		return nil, err
	}
	t.d.log.Info("started an agent for tasks", "source", name, "tickets", strings.Join(keys, " "), "agent", a.GetId())
	return a, nil
}

// renew extends the leases that end soon. A message whose lease is gone is
// dropped: the tracker hands it out again, and its events done are seen.
func (t *taskManager) renew(ctx context.Context) {
	for _, ts := range t.list() {
		if ts.src == nil {
			continue
		}
		name := ts.conf().SourceName()
		t.mu.Lock()
		var due []*leasedMsg
		for _, l := range t.state.Sources[name].InFlight {
			if time.Until(time.UnixMilli(l.LeaseMs)) < taskRenew {
				due = append(due, l)
			}
		}
		t.mu.Unlock()
		for _, l := range due {
			t.mu.Lock()
			handle := l.Handle
			t.mu.Unlock()
			err := ts.src.Extend(ctx, handle, taskLease)
			t.mu.Lock()
			switch {
			case err == nil:
				l.LeaseMs = time.Now().Add(taskLease).UnixMilli()
			case errors.Is(err, tasks.ErrGone):
				t.d.log.Warn("lost the lease on a task message; the tracker hands it out again", "source", name, "message", l.ID)
				t.dropLocked(name, l)
			default:
				t.d.log.Warn("renewing the lease on a task message failed", "source", name, "message", l.ID, "err", err)
			}
			t.saveLocked()
			t.mu.Unlock()
		}
	}
}

func (t *taskManager) dropLocked(name string, l *leasedMsg) {
	st := t.state.Sources[name]
	st.InFlight = slices.DeleteFunc(st.InFlight, func(o *leasedMsg) bool { return o == l })
}

// finish marks the groups whose agent is done with its turn, and
// acknowledges the messages whose groups are all done. A message with a
// group that could not start is left, once the others are done, for its
// lease to run out.
func (t *taskManager) finish(ctx context.Context) {
	now := time.Now()
	for _, ts := range t.list() {
		if ts.src == nil {
			continue
		}
		name := ts.conf().SourceName()
		t.mu.Lock()
		var acks, drops []*leasedMsg
		changed := false
		for _, l := range t.state.Sources[name].InFlight {
			over, failed := true, false
			for _, g := range l.Groups {
				if g.Started && !g.Done {
					busy, done := t.d.agents.taskAgentDone(g.Agent)
					if busy && !g.Busy {
						g.Busy, changed = true, true
					}
					if done && (g.Busy || now.Sub(time.UnixMilli(g.StartedMs)) >= taskIdleDone) {
						g.Done, changed = true, true
					}
				}
				switch {
				case g.Failed:
					failed = true
				case !g.Done:
					over = false
				}
			}
			switch {
			case over && failed:
				drops = append(drops, l)
			case over:
				acks = append(acks, l)
			}
		}
		for _, l := range drops {
			t.d.log.Warn("left a task message to the tracker: an agent could not start", "source", name, "message", l.ID)
			t.dropLocked(name, l)
			changed = true
		}
		if changed {
			t.saveLocked()
		}
		t.mu.Unlock()
		for _, l := range acks {
			t.mu.Lock()
			handle := l.Handle
			t.mu.Unlock()
			err := ts.src.Ack(ctx, handle)
			t.mu.Lock()
			if err == nil || errors.Is(err, tasks.ErrGone) {
				t.dropLocked(name, l)
				t.saveLocked()
				t.d.log.Info("task message done", "source", name, "message", l.ID)
			} else {
				t.d.log.Warn("acknowledging a task message failed", "source", name, "message", l.ID, "err", err)
			}
			t.mu.Unlock()
		}
	}
}

// closeDone closes the agents whose tickets are all done once they have
// been idle for their source's close_after. It archives them as the
// dashboard does: the session ends, its chat and its worktree stay. An
// agent with a ticket that is not done, or that cannot be looked up, stays.
func (t *taskManager) closeDone(ctx context.Context) {
	now := time.Now()
	live := t.d.agents.taskAgents()
	type closer struct {
		src   tasks.Source
		after time.Duration // 0: never
	}
	t.mu.Lock()
	srcs := map[string]closer{}
	for _, ts := range t.sources {
		if ts.src != nil {
			srcs[ts.conf().SourceName()] = closer{ts.src, ts.conf().CloseAfterOrDefault()}
		}
	}
	changed := false
	var due []taskIdler
	for _, a := range live {
		since, ok := t.state.Idle[a.ID]
		switch {
		case !a.Idle:
			if ok {
				delete(t.state.Idle, a.ID)
				changed = true
			}
			delete(t.checked, a.ID)
		case !ok:
			t.state.Idle[a.ID], changed = now.UnixMilli(), true
		case srcs[a.Source].after > 0 && now.Sub(time.UnixMilli(since)) >= srcs[a.Source].after &&
			now.Sub(t.checked[a.ID]) >= taskCloseCheck:
			t.checked[a.ID] = now
			due = append(due, a)
		}
	}
	for id := range t.state.Idle {
		if !slices.ContainsFunc(live, func(a taskIdler) bool { return a.ID == id }) {
			delete(t.state.Idle, id)
			delete(t.checked, id)
			changed = true
		}
	}
	if changed {
		t.saveLocked()
	}
	t.mu.Unlock()
	for _, a := range due {
		c := srcs[a.Source]
		if !t.ticketsDone(ctx, c.src, a) || !t.d.agents.taskAgentIdle(a.ID) {
			continue
		}
		log := t.d.log.With("source", a.Source, "agent", a.ID, "tickets", strings.Join(a.Keys, " "))
		if _, err := t.d.agents.kill(ctx, &fleetv1.KillAgentRequest{Agent: a.ID}); err != nil {
			log.Warn("could not close a task agent whose tickets are done", "err", err)
			continue
		}
		log.Info("closed a task agent: its tickets are done and it was idle", "close_after", c.after)
	}
}

// ticketsDone reports whether every ticket of a is done.
func (t *taskManager) ticketsDone(ctx context.Context, src tasks.Source, a taskIdler) bool {
	for _, key := range a.Keys {
		is, err := src.Issue(ctx, key)
		if err != nil {
			t.d.log.Debug("could not look up a ticket to close its agent", "source", a.Source, "issue", key, "err", err)
			return false
		}
		if !is.Done {
			return false
		}
	}
	return true
}

// heartbeats tells each tracker what its account's agents do.
func (t *taskManager) heartbeats(ctx context.Context) {
	now := time.Now()
	for _, ts := range t.list() {
		if ts.src == nil {
			continue
		}
		p := t.d.agents.taskPresence(ts.conf().SourceName())
		t.mu.Lock()
		ready := ts.me != nil
		changed := p != ts.beat
		due := changed || p.State != tasks.Idle && now.Sub(ts.beatAt) >= taskBeat
		// Nothing to say until an agent of the source did something.
		if !ready || !due || ts.beatAt.IsZero() && p.State == tasks.Idle {
			t.mu.Unlock()
			continue
		}
		t.mu.Unlock()
		if err := ts.src.Heartbeat(ctx, p); err != nil {
			t.d.log.Debug("task heartbeat failed", "source", ts.conf().SourceName(), "err", err)
			continue
		}
		t.mu.Lock()
		ts.beat, ts.beatAt = p, now
		t.mu.Unlock()
	}
}

// goIdle tells the trackers the account stopped, as the daemon stops.
func (t *taskManager) goIdle() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, ts := range t.list() {
		t.mu.Lock()
		busy := ts.src != nil && !ts.beatAt.IsZero() && ts.beat.State != tasks.Idle
		t.mu.Unlock()
		if busy {
			_ = ts.src.Heartbeat(ctx, tasks.Presence{State: tasks.Idle})
		}
	}
}

// listSources reports the sources for ListTaskSources.
func (t *taskManager) listSources() []*fleetv1.TaskSource {
	cfg := t.d.config()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*fleetv1.TaskSource, 0, len(t.sources))
	for _, ts := range t.sources {
		name := ts.conf().SourceName()
		p := &fleetv1.TaskSource{
			Name: name, Kind: ts.conf().Kind, Url: ts.conf().URL, Disabled: ts.conf().Disabled,
			Error: ts.err, MaxAgents: int32(ts.conf().MaxAgentsOrDefault()),
			CloseAfterMs: ts.conf().CloseAfterOrDefault().Milliseconds(),
			Working:      int32(t.d.agents.taskWorking(name)),
			Webhook:      ts.queue,
		}
		if p.Webhook == "" {
			p.Webhook = ts.conf().Webhook
		}
		if ts.me != nil {
			p.Account = ts.me.Handle
		}
		if !ts.lastPoll.IsZero() {
			p.LastPollMs = ts.lastPoll.UnixMilli()
		}
		st := t.state.Sources[name]
		p.InFlight = int32(len(st.InFlight))
		for _, l := range st.InFlight {
			for _, g := range l.Groups {
				if g.pending() {
					p.Waiting = append(p.Waiting, g.keys()...)
				}
			}
		}
		for _, proj := range ts.conf().Projects {
			ac := ts.conf().Agent(proj)
			tp := &fleetv1.TaskProject{Key: proj.Key, Path: proj.Path, Adapter: ac.Adapter}
			root, rel, abs := proj.ProjectDir()
			if _, dir, err := cfg.Resolve(root, rel, abs); err != nil {
				tp.Error = err.Error()
			} else {
				tp.Path = dir
			}
			if _, ok := t.d.opts.Adapters.Get(ac.Adapter); !ok && tp.Error == "" {
				tp.Error = fmt.Sprintf("unknown adapter %q", ac.Adapter)
			}
			p.Projects = append(p.Projects, tp)
		}
		out = append(out, p)
	}
	return out
}

// sourceFor returns the source named name ("" for the only one), ready to
// run.
func (t *taskManager) sourceFor(name string) (*taskSource, error) {
	var ts *taskSource
	for _, s := range t.list() {
		if name == "" || s.conf().SourceName() == name {
			if ts != nil {
				return nil, errf(codeInvalid, "there are several task sources; name one")
			}
			ts = s
		}
	}
	switch {
	case ts == nil && name == "":
		return nil, errf(codeNotFound, "no task source is set up ([[tasks]] in config.toml)")
	case ts == nil:
		return nil, errf(codeNotFound, "no task source %q", name)
	case ts.conf().Disabled:
		return nil, errf(codeUnavailable, "task source %s is disabled", ts.conf().SourceName())
	case ts.src == nil:
		t.mu.Lock()
		defer t.mu.Unlock()
		return nil, errf(codeUnavailable, "task source %s cannot run: %s", ts.conf().SourceName(), ts.err)
	}
	return ts, nil
}

// startTask starts an agent on a ticket now (StartTaskRequest), whatever
// max_agents says: a person asked for it.
func (t *taskManager) startTask(ctx context.Context, source, key string) (*fleetv1.Agent, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return nil, errf(codeInvalid, "a ticket key is required")
	}
	ts, err := t.sourceFor(source)
	if err != nil {
		return nil, err
	}
	name := ts.conf().SourceName()
	proj := projectOfKey(key)
	if _, ok := ts.conf().Project(proj); !ok {
		return nil, errf(codeInvalid, "project %s is mapped to no folder: add a [[tasks.project]] with key = %q", proj, proj)
	}
	t.startMu.Lock()
	defer t.startMu.Unlock()
	if id := t.d.agents.taskAgent(name, key); id != "" {
		return t.d.agents.agentProto(id)
	}
	ev := tasks.Event{Kind: tasks.Assigned, Project: proj, Issue: key, Text: "You were put on this ticket from fleet.", At: time.Now()}
	a, err := t.startTickets(ctx, ts, []tasks.Event{ev}, "", "")
	if errors.Is(err, tasks.ErrAuth) {
		return nil, errf(codeUnavailable, "%v", err)
	}
	return a, err
}

// setupQueue creates the source's queue for its mapped projects
// (SetupTaskSourceRequest), unless it has one, whose projects it brings in
// line instead.
func (t *taskManager) setupQueue(ctx context.Context, source string, quiet, maxWait time.Duration) (string, bool, string, error) {
	ts, err := t.sourceFor(source)
	if err != nil {
		return "", false, "", err
	}
	qc, ok := ts.src.(tasks.QueueCreator)
	if !ok {
		return "", false, "", errf(codeInvalid, "%s cannot create its queue: set it up in the tracker", ts.conf().Kind)
	}
	id, created, err := qc.CreateQueue(ctx, tasks.QueueOptions{Projects: mappedKeys(ts.conf()), Quiet: quiet, Max: maxWait})
	if err != nil {
		if errors.Is(err, tasks.ErrAuth) || errors.Is(err, tasks.ErrNoQueue) {
			return "", false, "", errf(codeUnavailable, "%v", err)
		}
		return "", false, "", errf(codeInvalid, "%v", err)
	}
	note := ""
	if !created {
		note = t.syncQueue(ctx, ts)
	}
	t.mu.Lock()
	ts.queue = id
	if strings.Contains(ts.err, tasks.ErrNoQueue.Error()) {
		ts.err = ""
	}
	t.mu.Unlock()
	select {
	case ts.kick <- struct{}{}:
	default:
	}
	return id, created, note, nil
}

// mappedKeys are the keys of the projects c maps, upper case.
func mappedKeys(c config.TaskSource) []string {
	var keys []string
	for _, p := range c.Projects {
		keys = append(keys, strings.ToUpper(p.Key))
	}
	return keys
}

// syncQueue brings the projects of ts's queue in line with the mapped ones
// and says what happened, "" when nothing did.
func (t *taskManager) syncQueue(ctx context.Context, ts *taskSource) string {
	qs, ok := ts.src.(tasks.QueueSyncer)
	if !ok || ts.src == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keys := mappedKeys(ts.conf())
	changed, err := qs.SyncQueue(ctx, keys)
	switch {
	case errors.Is(err, tasks.ErrNoQueue):
		return "the account has no queue yet: run `fleet tasks setup`"
	case err != nil:
		return "could not update the queue's projects: " + err.Error()
	case changed:
		id, _ := ts.src.Queue(ctx)
		return fmt.Sprintf("%s now carries the events of %s", id, strings.Join(keys, ", "))
	}
	return ""
}

func projectOfKey(key string) string {
	if i := strings.LastIndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}

// isCode reports whether err is an API error with code.
func isCode(err error, code fleetv1.ErrorCode) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.code == code
}

// taskAgent returns the ID of the newest live agent working ticket key of
// source, "" if none.
func (m *manager) taskAgent(source, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *agentRec
	for _, a := range m.agents {
		if a.live() && a.Task.works(source, key) && (best == nil || a.CreatedAtMs > best.CreatedAtMs) {
			best = a
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// taskBusy reports whether a task agent is busy with its tickets: not done
// with its turn.
func taskBusy(a *agentRec) bool {
	return a.live() && a.State != stateIdle
}

// taskWorking counts the live agents of source busy with their tickets.
func (m *manager) taskWorking(source string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.agents {
		if a.Task != nil && a.Task.Source == source && taskBusy(a) {
			n++
		}
	}
	return n
}

// taskIdler is a live agent started for tickets, as closeDone sees it.
type taskIdler struct {
	ID, Source string
	Keys       []string
	// Idle: done with its turn, with nothing queued for it.
	Idle bool
}

// taskAgents lists the live agents started for tickets.
func (m *manager) taskAgents() []taskIdler {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []taskIdler
	for _, a := range m.agents {
		if a.Task != nil && a.live() {
			out = append(out, taskIdler{ID: a.ID, Source: a.Task.Source, Keys: slices.Clone(a.Task.keys()), Idle: taskIdle(a)})
		}
	}
	return out
}

// taskAgentIdle reports whether agent id is live and idle (taskIdle).
func (m *manager) taskAgentIdle(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	return ok && taskIdle(a)
}

// taskIdle: a is done with its turn, with nothing queued for it (not even
// held), and RunAgent or KillAgent is not at work on it.
func taskIdle(a *agentRec) bool {
	return a.live() && a.State == stateIdle && len(a.Queue) == 0 && !a.busy
}

// taskAgentDone reports whether agent id is busy, and whether it is done:
// idle with nothing queued to send, or gone.
func (m *manager) taskAgentDone(id string) (busy, done bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok || !a.live() {
		return false, true
	}
	if taskBusy(a) {
		return true, false
	}
	return false, len(a.Queue) == 0 || a.QueuePaused
}

// taskPresence is the presence of source's account: working on the ticket
// of its most recently active working agent, else waiting while one needs
// a person's input, else idle.
func (m *manager) taskPresence(source string) tasks.Presence {
	m.mu.Lock()
	defer m.mu.Unlock()
	var work, wait *agentRec
	for _, a := range m.agents {
		if a.Task == nil || a.Task.Source != source || !a.live() {
			continue
		}
		switch a.State {
		case stateStarting, stateRunning, stateWorking:
			if work == nil || a.UpdatedAtMs > work.UpdatedAtMs {
				work = a
			}
		case stateNeedsInput:
			if wait == nil || a.UpdatedAtMs > wait.UpdatedAtMs {
				wait = a
			}
		}
	}
	switch {
	case work != nil:
		return tasks.Presence{State: tasks.Working, Issue: work.Task.Key, Summary: work.StateDetail}
	case wait != nil:
		return tasks.Presence{State: tasks.Waiting, Issue: wait.Task.Key, Summary: "waiting for input in fleet"}
	}
	return tasks.Presence{State: tasks.Idle}
}

// agentProto returns agent id as sent to clients.
func (m *manager) agentProto(id string) (*fleetv1.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil, errf(codeNotFound, "no agent %q", id)
	}
	return a.proto(), nil
}

// queueTaskNews queues text for live agent id, as a person would.
func (m *manager) queueTaskNews(id, text string) error {
	if r := []rune(text); len(r) > maxQueued {
		text = string(r[:maxQueued-60]) + "\n… (cut short: read the rest on the ticket)"
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "?")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok || !a.live() {
		return errf(codeNotFound, "agent %s is not running", id)
	}
	if len(a.Queue) >= maxQueue {
		return errf(codeInvalid, "%d messages are queued already", len(a.Queue))
	}
	a.Queue = append(a.Queue, queued{ID: queueID(a.Queue), Text: text, AddedAtMs: time.Now().UnixMilli()})
	m.queueChangedLocked(a)
	return nil
}
