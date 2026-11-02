package hub

import (
	"encoding/json"
	"net/http"
	"time"

	"fleet/internal/team"
)

// event appends e to the team's log and wakes long polls. Called with the
// server lock held.
func (t *teamState) event(e team.Event, now time.Time) {
	e.Seq, e.Time = t.NextEvent, now
	t.NextEvent++
	t.Events = append(t.Events, e)
	if len(t.Events) > maxEvents {
		t.Events = t.Events[len(t.Events)-maxEvents:]
	}
	if t.wake == nil {
		t.wake = make(chan struct{})
	}
	close(t.wake)
	t.wake = make(chan struct{})
}

// visible reports whether member m (admin or not) may see e: join and
// leave events are for admins, key events for the member they are for.
func visible(e team.Event, m team.Member) bool {
	switch e.Kind {
	case team.EventJoin, team.EventLeft:
		return m.Admin
	case team.EventKeys:
		return m.Admin || e.Member == m.ID
	}
	return true
}

// handleEvents long-polls the team's events after ?since. It releases the
// server lock while waiting.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, a *authed) {
	since := queryUint(r, "since")
	t := a.team
	out, wake := t.eventsAfter(since, a.member)
	if len(out.Events) == 0 && !out.Reset {
		s.mu.Unlock()
		select {
		case <-wake:
		case <-time.After(pollWait):
		case <-r.Context().Done():
		}
		s.mu.Lock()
		out, _ = t.eventsAfter(since, a.member)
	}
	writeJSON(w, http.StatusOK, out)
}

// eventsAfter is the events after since that m may see, and the channel
// that closes on the next event. Called with the server lock held.
func (t *teamState) eventsAfter(since uint64, m team.Member) (team.EventList, <-chan struct{}) {
	out := team.EventList{Events: []team.Event{}, Next: since}
	if t.wake == nil {
		t.wake = make(chan struct{})
	}
	if len(t.Events) > 0 && since > 0 && since < t.Events[0].Seq-1 {
		// The log no longer reaches back to the cursor.
		out.Reset, out.Next = true, t.NextEvent-1
		return out, t.wake
	}
	for _, e := range t.Events {
		if e.Seq <= since {
			continue
		}
		if visible(e, m) {
			out.Events = append(out.Events, e)
		}
		out.Next = e.Seq
	}
	return out, t.wake
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, team.Error{Error: msg})
}
