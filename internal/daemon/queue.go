package daemon

// Message queues: the user lines up messages for a live agent, and the
// daemon types the next one into its session once the agent is done with
// its turn (idle for queueSettle, no dialog, no usage limit, no workflow
// or subagent running in its background). Interrupting a turn pauses the
// queue, so the next message does not undo the Stop.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/workflow"
)

// Variables for tests.
var (
	// queueInterval is how often live agents are checked for a queued
	// message to send.
	queueInterval = 2 * time.Second
	// queueSettle is how long an agent must stay idle before the next
	// message goes in: a background task that ended starts the next turn
	// right after the last one.
	queueSettle = 3 * time.Second
	// queueRetry is how long a message may take to show (the agent leaves
	// idle) before the next one is sent anyway.
	queueRetry = time.Minute
)

const (
	// maxQueue caps an agent's queue, maxQueued one message, in characters.
	maxQueue  = 50
	maxQueued = 10000
	// queueStaleRun is how long a background run may show no activity and
	// still hold the queue: one whose agents died with the session never
	// ends.
	queueStaleRun = 30 * time.Minute
	// queueTyping bounds typing one message into a session.
	queueTyping = 10 * time.Second
)

// queued is a message in an agent's queue.
type queued struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	AddedAtMs int64  `json:"added_at_ms"`
}

func queueProto(q []queued) []*fleetv1.QueuedMessage {
	if len(q) == 0 {
		return nil
	}
	out := make([]*fleetv1.QueuedMessage, len(q))
	for i, m := range q {
		out[i] = &fleetv1.QueuedMessage{Id: m.ID, Text: m.Text, AddedAtMs: m.AddedAtMs}
	}
	return out
}

// editQueue applies req to the queue of a live agent and returns the agent.
func (m *manager) editQueue(req *fleetv1.EditQueueRequest) (*fleetv1.Agent, error) {
	var text string
	if op, ok := req.GetOp().(*fleetv1.EditQueueRequest_Add); ok {
		text = strings.TrimSpace(op.Add)
		if text == "" {
			return nil, errf(codeInvalid, "the message is empty")
		}
		if !utf8.ValidString(text) {
			return nil, errf(codeInvalid, "the message is not valid UTF-8")
		}
		if n := utf8.RuneCountInString(text); n > maxQueued {
			return nil, errf(codeInvalid, "the message is %d characters long, at most %d can be queued", n, maxQueued)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.find(req.GetAgent())
	if err != nil {
		return nil, err
	}
	if !a.live() {
		return nil, errf(codeInvalid, "agent %s is not running", a.ID)
	}
	switch op := req.GetOp().(type) {
	case *fleetv1.EditQueueRequest_Add:
		if ad, _ := m.d.opts.Adapters.Get(a.Adapter); ad == nil || !ad.Capabilities().ActivityState {
			return nil, errf(codeInvalid, "agent %s reports no end of its turns: a queue would never move", a.ID)
		}
		if len(a.Queue) >= maxQueue {
			return nil, errf(codeInvalid, "%d messages are queued already, the most an agent can have", len(a.Queue))
		}
		a.Queue = append(a.Queue, queued{ID: queueID(a.Queue), Text: text, AddedAtMs: time.Now().UnixMilli()})
	case *fleetv1.EditQueueRequest_Remove:
		i := slices.IndexFunc(a.Queue, func(q queued) bool { return q.ID == op.Remove })
		if i < 0 {
			return nil, errf(codeNotFound, "no queued message %q", op.Remove)
		}
		a.Queue = slices.Delete(a.Queue, i, i+1)
	case *fleetv1.EditQueueRequest_Paused:
		a.QueuePaused = op.Paused
	default:
		return nil, errf(codeInvalid, "add, remove or paused is required")
	}
	if len(a.Queue) == 0 {
		a.QueuePaused = false
	}
	m.queueChangedLocked(a)
	return a.proto(), nil
}

// queueChangedLocked saves and broadcasts a's queue. Like a note, it is no
// news from the agent: UpdatedAtMs stays.
func (m *manager) queueChangedLocked(a *agentRec) {
	m.saveLocked()
	m.broadcastLocked(&fleetv1.Event{Kind: &fleetv1.Event_AgentUpserted{AgentUpserted: a.proto()}})
}

// queueID returns an id no message in q has.
func queueID(q []queued) string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if !slices.ContainsFunc(q, func(m queued) bool { return m.ID == id }) {
			return id
		}
	}
}

// queueLoop sends queued messages until ctx is cancelled.
func (m *manager) queueLoop(ctx context.Context) {
	t := time.NewTicker(queueInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.sendQueued(ctx)
	}
}

// queueReadyLocked reports whether agent a is done with its turn, as far
// as its record tells: idle, no dialog, not stopped by a usage limit.
func queueReadyLocked(a *agentRec) bool {
	return a.live() && !a.busy && a.State == stateIdle && len(a.Dialogs) == 0 && !a.ScreenPrompt && a.limit == nil
}

// sendQueued sends the next queued message of every agent that is done
// with its turn.
func (m *manager) sendQueued(ctx context.Context) {
	now := time.Now()
	var due []string
	m.mu.Lock()
	for id, a := range m.agents {
		if !queueReadyLocked(a) {
			a.idleSince, a.queueSent = time.Time{}, time.Time{}
			continue
		}
		// Anything the agent did since restarts the wait.
		if a.idleSince.IsZero() || a.UpdatedAtMs != a.idleAt {
			a.idleSince, a.idleAt, a.queueSent = now, a.UpdatedAtMs, time.Time{}
		}
		switch {
		case len(a.Queue) == 0 || a.QueuePaused:
		case now.Sub(a.idleSince) < queueSettle:
		case !a.queueSent.IsZero() && now.Sub(a.queueSent) < queueRetry:
		default:
			due = append(due, id)
		}
	}
	m.mu.Unlock()
	sort.Strings(due)
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		m.sendNext(ctx, id)
	}
}

// sendNext types the next queued message of agent id into its session,
// unless its session works in the background or shows a dialog.
func (m *manager) sendNext(ctx context.Context, id string) {
	if m.backgroundWork(id) {
		return
	}
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok || !queueReadyLocked(a) {
		m.mu.Unlock()
		return
	}
	session, adapterID := a.TmuxSession, a.Adapter
	m.mu.Unlock()
	if m.dialogOpen(ctx, session, adapterID) {
		return
	}

	// Taken off the queue before it is typed: removing it meanwhile must
	// not send it anyway.
	m.mu.Lock()
	a, ok = m.agents[id]
	if !ok || !queueReadyLocked(a) || len(a.Queue) == 0 || a.QueuePaused {
		m.mu.Unlock()
		return
	}
	next := a.Queue[0]
	a.Queue = slices.Delete(a.Queue, 0, 1)
	a.queueSent = time.Now()
	m.queueChangedLocked(a)
	m.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, queueTyping)
	defer cancel()
	if err := m.d.tmux.SendText(sctx, session, next.Text, true); err != nil {
		m.d.log.Warn("sending a queued message failed", "agent", id, "err", err)
		m.mu.Lock()
		if a, ok := m.agents[id]; ok && a.live() && len(a.Queue) < maxQueue {
			a.Queue = slices.Insert(a.Queue, 0, next)
			m.queueChangedLocked(a)
		}
		m.mu.Unlock()
		return
	}
	m.d.log.Info("sent a queued message", "agent", id)
}

// backgroundWork reports whether the session of agent id runs subagents or
// workflows in the background, which start the next turn when they end.
func (m *manager) backgroundWork(id string) bool {
	_, runs, err := m.workflows(id)
	if err != nil {
		return false
	}
	since := time.Now().Add(-queueStaleRun).UnixMilli()
	return slices.ContainsFunc(runs, func(r workflow.Run) bool {
		return r.Status == workflow.Running && max(r.UpdatedMs, r.StartedMs) >= since
	})
}

// pauseQueueLocked pauses a's queue, if anything is queued.
func (m *manager) pauseQueueLocked(a *agentRec) {
	if len(a.Queue) > 0 && !a.QueuePaused {
		a.QueuePaused = true
		m.queueChangedLocked(a)
	}
}
