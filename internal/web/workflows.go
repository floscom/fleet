package web

// Workflows: the scripts an agent runs in the background of its session,
// each starting many subagents in phases (see workflow.Run). Admin routes
// like the rest of /api/, and forwarded to other fleets.
//
//	GET /api/workflows                                   runs going on now
//	GET /api/agents/{id}/workflows                       an agent's runs
//	GET /api/agents/{id}/workflows/{run}/agents/{sub}/chat
//	                                                     one subagent's
//	                                                     conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/workflow"
)

const (
	// workflowPoll is how often a waiting workflows request looks for news.
	// Agents of a run report something every few seconds; looking takes
	// reading what they appended.
	workflowPoll = time.Second
	// recentRun is how long a run that ended stays in GET /api/workflows.
	recentRun = 10 * time.Minute
)

// activeRun is a run of GET /api/workflows.
type activeRun struct {
	// Agent is the id of the agent whose session runs it.
	Agent string       `json:"agent"`
	Run   workflow.Run `json:"run"`
}

// apiWorkflows lists the runs of live agents that go on, or ended within
// recentRun, without their results and logs.
func (s *Server) apiWorkflows(w http.ResponseWriter, r *http.Request) {
	out := []activeRun{}
	since := time.Now().Add(-recentRun).UnixMilli()
	for _, a := range s.opts.Source.Agents() {
		if a.State == fleetv1.AgentState_AGENT_STATE_EXITED || a.State == fleetv1.AgentState_AGENT_STATE_FAILED {
			continue
		}
		_, runs, err := s.opts.Source.Workflows(a.Id)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if run.Status == workflow.Running || run.EndedMs >= since {
				run.Result, run.Logs = "", nil
				out = append(out, activeRun{Agent: a.Id, Run: run})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string][]activeRun{"workflows": out})
}

// workflowsReply is the answer of GET /api/agents/{id}/workflows.
type workflowsReply struct {
	Agent Agent          `json:"agent"`
	Runs  []workflow.Run `json:"runs"`
	// V names this state of the runs and the agent: pass it back with
	// wait=1 to hear of the next.
	V string `json:"v"`
}

// apiAgentWorkflows serves an agent's workflow runs, oldest first. With
// wait=1&v=<the V the page has>, it holds out until they changed, or
// chatWait passed.
func (s *Server) apiAgentWorkflows(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	wait, seen := q.Get("wait") == "1", q.Get("v")
	deadline := time.Now().Add(chatWait)
	for {
		a, runs, err := s.opts.Source.Workflows(id)
		if err != nil {
			s.writeSourceError(w, err)
			return
		}
		reply := workflowsReply{Agent: agentOf(a), Runs: nonNil(runs)}
		reply.V = version(reply)
		if !wait || reply.V != seen || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, reply)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(min(workflowPoll, time.Until(deadline))):
		}
	}
}

// apiWorkflowChat serves the conversation of one agent of a workflow run,
// like apiChat.
func (s *Server) apiWorkflowChat(w http.ResponseWriter, r *http.Request) {
	id, run, sub := r.PathValue("id"), r.PathValue("run"), r.PathValue("sub")
	s.serveChat(w, r, func() (Chat, error) { return s.opts.Source.WorkflowChat(id, run, sub) })
}

func (s *Server) apiWorkflowImage(w http.ResponseWriter, r *http.Request) {
	id, run, sub := r.PathValue("id"), r.PathValue("run"), r.PathValue("sub")
	s.serveImage(w, r, func() (Chat, error) { return s.opts.Source.WorkflowChat(id, run, sub) })
}

// version names what v says, short.
func version(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
