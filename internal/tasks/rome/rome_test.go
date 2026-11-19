package rome

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/internal/tasks"
)

const testToken = "rome_sat_s3cr3t"

// fakeRome serves the endpoints the source uses: one account (ada), its
// pull webhooks, and ROME-7.
type fakeRome struct {
	t        *testing.T
	mu       sync.Mutex
	hooks    []map[string]any // GET /webhooks items
	messages []map[string]any // handed out by the next receive
	receive  map[string]any   // last receive body
	extended []map[string]any
	deleted  []map[string]any
	failCode string // entry failure code of visibility/delete
	created  map[string]any
	idemKey  string
	beats    []map[string]any
	status   int // answer every request with it when set
	// projectIDs are whk_1's project_ids; patched its last PATCH.
	projectIDs []string
	patched    map[string]any
}

func (f *fakeRome) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.status
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		status = http.StatusUnauthorized
	}
	if status != 0 {
		w.Header().Set("Content-Type", "application/problem+json")
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"status": status, "code": "token-revoked", "detail": "the token was revoked", "instance": "req_1"})
		return
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&body)
	}
	entry := func() map[string]any {
		e, _ := body["entries"].([]any)
		m, _ := e[0].(map[string]any)
		return m
	}
	answerEntry := func() {
		id := entry()["id"]
		if f.failCode != "" {
			json.NewEncoder(w).Encode(map[string]any{"successful": []any{}, "failed": []any{map[string]any{"id": id, "code": f.failCode, "message": "no"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"successful": []any{map[string]any{"id": id}}, "failed": []any{}})
	}
	switch p := r.URL.Path; {
	case p == "/api/v1/me":
		json.NewEncoder(w).Encode(map[string]any{"id": "usr_ada", "handle": "ada", "name": "Ada", "kind": "service"})
	case p == "/api/v1/webhooks" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{"items": f.hooks, "next_cursor": nil})
	case p == "/api/v1/webhooks" && r.Method == http.MethodPost:
		f.created, f.idemKey = body, r.Header.Get("Idempotency-Key")
		hook := pullHook("whk_new", "usr_ada")
		f.hooks = append(f.hooks, hook)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(hook)
	case p == "/api/v1/webhooks/whk_1/receive-messages":
		f.receive = body
		msgs := f.messages
		if msgs == nil {
			msgs = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
		f.messages = nil
	case p == "/api/v1/webhooks/whk_1/change-message-visibility":
		f.extended = append(f.extended, entry())
		answerEntry()
	case p == "/api/v1/webhooks/whk_1/delete-messages":
		f.deleted = append(f.deleted, entry())
		answerEntry()
	case p == "/api/v1/webhooks/whk_1" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{"id": "whk_1", "project_ids": f.projectIDs, "version": 4})
	case p == "/api/v1/webhooks/whk_1" && r.Method == http.MethodPatch:
		json.NewDecoder(r.Body).Decode(&f.patched)
		json.NewEncoder(w).Encode(map[string]any{"id": "whk_1"})
	case p == "/api/v1/projects/ROME", p == "/api/v1/projects/WEB":
		key := strings.TrimPrefix(p, "/api/v1/projects/")
		json.NewEncoder(w).Encode(map[string]any{"id": "prj_" + strings.ToLower(key), "key": key})
	case p == "/api/v1/issues/ROME-7/comments":
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "cmt_1", "body": "@ada can you\nlook at this?"}}})
	case p == "/api/v1/issues/ROME-7":
		json.NewEncoder(w).Encode(map[string]any{
			"key": "ROME-7", "type": "task", "title": "Fix the badge", "priority": "high",
			"status": map[string]any{"name": "To Do"}, "project": map[string]any{"key": "ROME"},
			"description": "The badge is unreadable.", "reporter": map[string]any{"handle": "flo"},
			"labels": []string{"ui"}, "parent": map[string]any{"key": "ROME-1", "title": "Design pass"},
		})
	case p == "/api/v1/me/heartbeat":
		f.beats = append(f.beats, body)
		w.Write([]byte(`{}`))
	case p == "/api/v1/projects":
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"key": "ROME", "name": "Rome"}, {"key": "OLD", "name": "Old", "archived": true},
		}, "next_cursor": nil})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// pullHook is a pull webhook of owner.
func pullHook(id, owner string) map[string]any {
	return map[string]any{"id": id, "owner": map[string]any{"id": owner}, "destination": "pull", "state": "active"}
}

func newFake(t *testing.T, webhook string) (*fakeRome, *Source) {
	f := &fakeRome{t: t, hooks: []map[string]any{pullHook("whk_1", "usr_ada")}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New("rome", srv.URL+"/", testToken, webhook)
}

// ev is an event about ROME-7 as Rome sends it: only issue.* events have
// the ticket as their entity, and every one names it in data.issue.
func ev(id string, typ string, actor string, data map[string]any) map[string]any {
	entity := map[string]any{"type": "issue", "id": "iss_7", "key": "ROME-7"}
	switch {
	case strings.HasPrefix(typ, "mention."):
		entity = map[string]any{"type": "comment", "id": "cmt_1"}
	case strings.HasPrefix(typ, "ask."), strings.HasPrefix(typ, "approval."):
		entity = map[string]any{"type": "ask", "id": "ask_1"}
	}
	if data == nil {
		data = map[string]any{}
	}
	data["issue"] = map[string]any{"id": "iss_7", "key": "ROME-7", "title": "Fix the badge"}
	return map[string]any{
		"id": id, "seq": 1, "type": typ, "occurred_at": "2026-11-18T09:00:00.000Z", "channel": "ui",
		"actor":   map[string]any{"id": actor, "handle": strings.TrimPrefix(actor, "usr_")},
		"project": map[string]any{"key": "ROME"}, "entity": entity,
		"data": data,
	}
}

// message is a pull message whose body is a batch of events.
func message(id string, events ...map[string]any) map[string]any {
	return map[string]any{
		"message_id": id, "receipt_handle": id + ".h1",
		"body": map[string]any{
			"webhook": map[string]any{"id": "whk_1", "route": map[string]any{"folder": "/srv/app"}, "prompt": "Work them in order."},
			"trigger": "quiet", "events": events,
		},
		"attributes": map[string]any{"receive_count": 2, "event_count": len(events)},
	}
}

func TestReceiveConverts(t *testing.T) {
	f, s := newFake(t, "")
	assigned := func(id, to string) map[string]any {
		return ev(id, "issue.assigned", "usr_flo", map[string]any{"to": map[string]any{"id": to}})
	}
	imported := assigned("evt_8", "usr_ada")
	imported["channel"] = "import"
	f.messages = []map[string]any{message("msg_1",
		assigned("evt_1", "usr_ada"),
		assigned("evt_2", "usr_max"), // assigned to someone else
		ev("evt_3", "issue.assigned", "usr_ada", map[string]any{"to": map[string]any{"id": "usr_ada"}}), // its own doing
		ev("evt_4", "mention.created", "usr_flo", map[string]any{"comment_id": "cmt_1", "by": map[string]any{"handle": "flo"}}),
		ev("evt_5", "ask.answered", "usr_flo", map[string]any{
			"ask":    map[string]any{"id": "ask_1", "kind": "decision", "text": "Cursors or offsets?"},
			"answer": map[string]any{"by": map[string]any{"handle": "flo"}, "option_text": "Cursors", "comment": "we use them already"},
		}),
		ev("evt_6", "approval.decided", "usr_flo", map[string]any{"approval": map[string]any{"to": map[string]any{"name": "Done"}}, "outcome": "rejected", "reason": "tests fail"}),
		ev("evt_7", "approval.decided", "usr_flo", map[string]any{"approval": map[string]any{"to": map[string]any{"name": "Done"}}, "outcome": "approved"}),
		imported,
	)}

	msgs, err := s.Receive(context.Background(), 3, time.Minute, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if b := f.receive; b["max_messages"] != 3.0 || b["wait_seconds"] != 20.0 || b["visibility_timeout_seconds"] != 900.0 {
		t.Errorf("receive body = %v (wait is capped at 20 s)", b)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0]
	if m.ID != "msg_1" || m.Handle != "msg_1.h1" || m.ReceiveCount != 2 || m.Prompt != "Work them in order." || m.Route["folder"] != "/srv/app" {
		t.Errorf("message = %+v", m)
	}
	var ids []string
	for _, e := range m.Events {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, " "); got != "evt_1 evt_4 evt_5 evt_6 evt_7" {
		t.Fatalf("events = %s, want evt_1 evt_4 evt_5 evt_6 evt_7", got)
	}
	for _, e := range m.Events {
		if e.Issue != "ROME-7" || e.Project != "ROME" {
			t.Errorf("%s names %s of %s, want ROME-7 of ROME", e.ID, e.Issue, e.Project)
		}
	}
	if e := m.Events[0]; e.Kind != tasks.Assigned || !strings.Contains(e.Text, "@flo assigned") {
		t.Errorf("assigned = %+v", e)
	}
	if e := m.Events[1]; e.Kind != tasks.News || !e.Start || !strings.Contains(e.Text, "> @ada can you\n> look at this?") {
		t.Errorf("mention = %+v", e)
	}
	if e := m.Events[2]; !e.Start || !strings.Contains(e.Text, "> Cursors or offsets?") || !strings.Contains(e.Text, "> Cursors") || !strings.Contains(e.Text, "> we use them already") {
		t.Errorf("answer = %+v", e)
	}
	if e := m.Events[3]; !e.Start || !strings.Contains(e.Text, "rejected") || !strings.Contains(e.Text, "> tests fail") {
		t.Errorf("rejection = %+v", e)
	}
	if e := m.Events[4]; e.Start || !strings.Contains(e.Text, "approved") {
		t.Errorf("approval = %+v", e)
	}

	// Nothing ready: no messages, no error.
	if msgs, err := s.Receive(context.Background(), 1, time.Second, time.Minute); err != nil || len(msgs) != 0 {
		t.Errorf("empty receive = %v, %v", msgs, err)
	}
}

func TestQueue(t *testing.T) {
	ctx := context.Background()
	f, s := newFake(t, "")
	f.hooks = append(f.hooks, pullHook("whk_other", "usr_max"),
		map[string]any{"id": "whk_push", "owner": map[string]any{"id": "usr_ada"}, "destination": "push", "state": "active"})
	if id, err := s.Queue(ctx); err != nil || id != "whk_1" {
		t.Errorf("Queue() = %q, %v; want the account's only pull webhook", id, err)
	}

	f, s = newFake(t, "")
	f.hooks = append(f.hooks, pullHook("whk_2", "usr_ada"))
	if _, err := s.Queue(ctx); !errors.Is(err, tasks.ErrNoQueue) || !strings.Contains(err.Error(), "whk_1, whk_2") {
		t.Errorf("two pull webhooks: err = %v", err)
	}
	_, s = newFake(t, "whk_2")
	if id, err := s.Queue(ctx); err != nil || id != "whk_2" {
		t.Errorf("configured: Queue() = %q, %v", id, err)
	}

	f, s = newFake(t, "")
	f.hooks = nil
	if _, err := s.Receive(ctx, 1, time.Second, time.Minute); !errors.Is(err, tasks.ErrNoQueue) || !strings.Contains(err.Error(), "fleet tasks setup") {
		t.Errorf("no pull webhook: err = %v", err)
	}
	id, created, err := s.CreateQueue(ctx, tasks.QueueOptions{Projects: []string{"ROME"}, Quiet: 2 * time.Minute, Max: 15 * time.Minute})
	if err != nil || id != "whk_new" || !created {
		t.Fatalf("CreateQueue() = %q, %v, %v", id, created, err)
	}
	c := f.created
	if c["destination"] != "pull" || c["format"] != "batch" || c["only_mine"] != true || c["url"] != nil || c["secret"] != nil {
		t.Errorf("created = %v", c)
	}
	if pids, _ := c["project_ids"].([]any); len(pids) != 1 || pids[0] != "prj_rome" {
		t.Errorf("project_ids = %v", c["project_ids"])
	}
	if d, _ := c["delivery"].(map[string]any); d["mode"] != "collect" || d["quiet_minutes"] != 2.0 || d["max_minutes"] != 15.0 {
		t.Errorf("delivery = %v", c["delivery"])
	}
	if types, _ := c["event_types"].([]any); len(types) != len(EventTypes) {
		t.Errorf("event_types = %v", c["event_types"])
	}
	if f.idemKey == "" {
		t.Error("create sent no Idempotency-Key")
	}
	if id, created, err := s.CreateQueue(ctx, tasks.QueueOptions{}); err != nil || id != "whk_new" || created {
		t.Errorf("CreateQueue() again = %q, %v, %v; want the existing one", id, created, err)
	}
}

func TestExtendAck(t *testing.T) {
	ctx := context.Background()
	f, s := newFake(t, "whk_1")
	if err := s.Extend(ctx, "h1", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if e := f.extended[0]; e["receipt_handle"] != "h1" || e["visibility_timeout_seconds"] != 900.0 {
		t.Errorf("extended = %v", e)
	}
	if e := f.deleted[0]; e["receipt_handle"] != "h1" {
		t.Errorf("deleted = %v", e)
	}
	for _, code := range []string{"receipt-handle-invalid", "message-not-inflight"} {
		f.failCode = code
		if err := s.Extend(ctx, "h1", time.Minute); !errors.Is(err, tasks.ErrGone) {
			t.Errorf("%s: Extend err = %v, want ErrGone", code, err)
		}
		if err := s.Ack(ctx, "h1"); !errors.Is(err, tasks.ErrGone) {
			t.Errorf("%s: Ack err = %v, want ErrGone", code, err)
		}
	}
}

func TestErrors(t *testing.T) {
	f, s := newFake(t, "whk_1")
	if _, err := s.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.status = http.StatusTooManyRequests
	_, err := s.Receive(context.Background(), 1, time.Second, time.Minute)
	var re *tasks.RetryError
	if !errors.As(err, &re) || re.After != 7*time.Second {
		t.Errorf("429: err = %v, want a RetryError after 7s", err)
	}
	f.status = http.StatusUnauthorized
	_, err = s.Receive(context.Background(), 1, time.Second, time.Minute)
	if !errors.Is(err, tasks.ErrAuth) || !strings.Contains(err.Error(), "token-revoked") || !strings.Contains(err.Error(), "req_1") {
		t.Errorf("401: err = %v, want ErrAuth with the code and request id", err)
	}
}

func TestIssueProjectsHeartbeat(t *testing.T) {
	f, s := newFake(t, "")
	ctx := context.Background()
	is, err := s.Issue(ctx, "ROME-7")
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "Fix the badge" || is.Status != "To Do" || is.Reporter != "@flo" || is.Parent != "ROME-1 Design pass" || !strings.HasSuffix(is.URL, "/ROME-7") || strings.Contains(is.URL, "//ROME") {
		t.Errorf("issue = %+v", is)
	}
	ps, err := s.Projects(ctx)
	if err != nil || len(ps) != 1 || ps[0].Key != "ROME" {
		t.Errorf("projects = %+v, %v; want ROME only", ps, err)
	}
	long := strings.Repeat("x", 300)
	if err := s.Heartbeat(ctx, tasks.Presence{State: tasks.Working, Issue: "ROME-7", Summary: long}); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, tasks.Presence{State: tasks.Idle}); err != nil {
		t.Fatal(err)
	}
	if b := f.beats[0]; b["state"] != "working" || b["issue_key"] != "ROME-7" || len([]rune(b["summary"].(string))) != maxSummary {
		t.Errorf("working beat = %v", b)
	}
	if b := f.beats[1]; b["state"] != "idle" || b["issue_key"] != nil || b["summary"] != nil {
		t.Errorf("idle beat = %v", b)
	}
}

func TestSetup(t *testing.T) {
	_, s := newFake(t, "")
	st := s.Setup(tasks.Account{Handle: "ada", Owner: "flo"})
	if st.Env[EnvToken] != testToken || !strings.HasPrefix(st.Env[EnvURL], "http") || strings.HasSuffix(st.Env[EnvURL], "/") {
		t.Errorf("env = %v", st.Env)
	}
	if len(st.MCPServers) != 1 || st.MCPServers[0].Name != "rome" || !strings.HasSuffix(st.MCPServers[0].URL, "/mcp") || st.MCPServers[0].TokenEnv != EnvToken || !st.MCPServers[0].Trusted {
		t.Errorf("mcp = %+v", st.MCPServers)
	}
	if !strings.Contains(st.Instructions, "@ada") || !strings.Contains(st.Instructions, "private agent of @flo") || strings.Contains(st.Instructions, "s3cr3t") {
		t.Errorf("instructions = %s", st.Instructions)
	}
}

func TestSyncQueue(t *testing.T) {
	ctx := context.Background()
	f, s := newFake(t, "whk_1")
	// A webhook of every project is never narrowed... or widened.
	if changed, err := s.SyncQueue(ctx, []string{"ROME"}); err != nil || changed || f.patched != nil {
		t.Errorf("all projects: changed %v, %v, patched %v", changed, err, f.patched)
	}
	f.projectIDs = []string{"prj_rome"}
	if changed, err := s.SyncQueue(ctx, []string{"ROME"}); err != nil || changed {
		t.Errorf("same projects: changed %v, %v", changed, err)
	}
	if changed, err := s.SyncQueue(ctx, nil); err != nil || changed {
		t.Errorf("no projects: changed %v, %v; must leave the webhook alone", changed, err)
	}
	changed, err := s.SyncQueue(ctx, []string{"ROME", "WEB"})
	if err != nil || !changed {
		t.Fatalf("new project: changed %v, %v", changed, err)
	}
	if ids, _ := f.patched["project_ids"].([]any); len(ids) != 2 || ids[1] != "prj_web" || f.patched["version"] != 4.0 {
		t.Errorf("patched = %v", f.patched)
	}
}
