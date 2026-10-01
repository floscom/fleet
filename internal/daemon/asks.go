package daemon

// Questions an agent asks in a dialog of choices, answered on the
// dashboard: the hook that reported the dialog (`fleet hook --wait`) waits
// here for the answer, and prints what the adapter makes of it. The dialog
// stays open in the terminal meanwhile; answered there, it closes, and the
// waiting hook ends with nothing to print.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"time"

	"fleet/internal/adapter"
	"fleet/internal/ask"
)

// askPoll is how often a waiting hook checks that its dialog is still open.
const askPoll = 500 * time.Millisecond

// pendingAsk is a hook waiting for the answer to its agent's questions.
type pendingAsk struct {
	id    string
	agent string
	// by and call name the dialog it answers (see dialog).
	by, call string
	qs       []ask.Question
	ev       adapter.HookEvent
	asker    adapter.Asker
	// reply gets the hook's output once answered (buffered).
	reply chan []byte
}

// addAskLocked records that the hook of ev waits for the answer to qs,
// asked in a's dialog by/call.
func (m *manager) addAskLocked(a *agentRec, by, call string, qs []ask.Question, ev adapter.HookEvent, asker adapter.Asker) *pendingAsk {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	p := &pendingAsk{id: hex.EncodeToString(b), agent: a.ID, by: by, call: call, qs: qs, ev: ev, asker: asker, reply: make(chan []byte, 1)}
	if m.asks == nil {
		m.asks = map[string]*pendingAsk{}
	}
	m.asks[p.id] = p
	return p
}

// awaitAnswer waits until p is answered, its dialog closed (answered in
// the terminal, refused, the agent ended), or ctx is done, and returns
// what the hook prints: the answer's output, or nothing.
func (m *manager) awaitAnswer(ctx context.Context, p *pendingAsk) []byte {
	defer func() {
		m.mu.Lock()
		delete(m.asks, p.id)
		m.mu.Unlock()
	}()
	t := time.NewTicker(askPoll)
	defer t.Stop()
	for {
		select {
		case out := <-p.reply:
			return out
		case <-ctx.Done():
			return nil
		case <-t.C:
			m.mu.Lock()
			open := m.askOpenLocked(p)
			m.mu.Unlock()
			if !open {
				return nil
			}
		}
	}
}

// askOpenLocked reports whether p's dialog is still open.
func (m *manager) askOpenLocked(p *pendingAsk) bool {
	a, ok := m.agents[p.agent]
	return ok && a.live() && m.asks[p.id] == p && dialogIndex(a, p) >= 0
}

func dialogIndex(a *agentRec, p *pendingAsk) int {
	return slices.IndexFunc(a.Dialogs, func(d dialog) bool { return d.By == p.by && d.Call == p.call })
}

// asksOf lists the questions agent id waits on, in the order the CLI
// shows its dialogs.
func (m *manager) asksOf(id string) []ask.Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil
	}
	type pos struct {
		i int
		p *pendingAsk
	}
	var open []pos
	for _, p := range m.asks {
		if p.agent != id || !a.live() {
			continue
		}
		if i := dialogIndex(a, p); i >= 0 {
			open = append(open, pos{i, p})
		}
	}
	slices.SortFunc(open, func(x, y pos) int { return x.i - y.i })
	out := make([]ask.Pending, 0, len(open))
	for _, o := range open {
		out = append(out, ask.Pending{ID: o.p.id, Questions: o.p.qs})
	}
	return out
}

// answer answers the questions id of agent ref.
func (m *manager) answer(ref, id string, ans ask.Answer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.find(ref)
	if err != nil {
		return err
	}
	p, ok := m.asks[id]
	if !ok || p.agent != a.ID || !m.askOpenLocked(p) {
		return errf(codeNotFound, "the question was answered or is gone")
	}
	if err := ans.Check(p.qs); err != nil {
		return errf(codeInvalid, "%v", err)
	}
	out, err := p.asker.AnswerOutput(p.ev, ans)
	if err != nil {
		return err
	}
	delete(m.asks, id)
	p.reply <- out
	return nil
}
