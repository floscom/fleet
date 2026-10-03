// Package workflow describes the workflows an agent CLI runs: scripts that
// start many subagents, in phases, in the background of a session (Claude
// Code's Workflow tool). Adapters that know where their CLI records them
// read them (adapter.Workflower); the dashboard shows them.
package workflow

import "slices"

// Run and agent statuses. A run is Running, Completed, Failed or Stopped;
// an agent of it Running, Done, Failed or Stopped.
const (
	Running   = "running"
	Completed = "completed"
	Done      = "done"
	Failed    = "failed"
	// Stopped means it ended without finishing: the run was killed, or the
	// session ended while it ran.
	Stopped = "stopped"
)

// Run is one run of a workflow script.
type Run struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	// Phases are the phases the script declares, in order, then any phase
	// only its agents name.
	Phases []Phase `json:"phases"`
	// Phase is the current phase: the one the latest running agent is in,
	// or the latest agent's once none runs.
	Phase string `json:"phase,omitempty"`
	// Agents are in the order they started.
	Agents []Agent `json:"agents"`
	Counts Counts  `json:"counts"`
	// Tokens and ToolUses add up the agents'.
	Tokens   int64 `json:"tokens"`
	ToolUses int   `json:"toolUses"`
	// Times in ms since the epoch; 0 if unknown. UpdatedMs is the latest
	// activity of any agent.
	StartedMs int64 `json:"startedMs,omitempty"`
	UpdatedMs int64 `json:"updatedMs,omitempty"`
	EndedMs   int64 `json:"endedMs,omitempty"`
	// Summary is how the CLI summed up the run's end, with the error of a
	// failed one.
	Summary string `json:"summary,omitempty"`
	// Result is what the script returned (often JSON), clipped.
	Result string `json:"result,omitempty"`
	// Logs are the lines the script logged, known once it ended.
	Logs []string `json:"logs,omitempty"`
}

// Phase is a phase a workflow script declares.
type Phase struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// Agent is one subagent of a run.
type Agent struct {
	ID string `json:"id"`
	// Label is the name the script gave it, "" if none.
	Label  string `json:"label,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Status string `json:"status"`
	// Call is the id of the tool call that started it, if known.
	Call  string `json:"call,omitempty"`
	Model string `json:"model,omitempty"`
	// Tokens is the size of its conversation: the context of its latest
	// message plus that message's output.
	Tokens   int64 `json:"tokens"`
	ToolUses int   `json:"toolUses"`
	// Tool and Activity are what it did last: the tool it called and a
	// one-line summary of the call, or (Tool "") the start of its latest
	// text. A finished agent's latest text is mostly its result.
	Tool      string `json:"tool,omitempty"`
	Activity  string `json:"activity,omitempty"`
	StartedMs int64  `json:"startedMs,omitempty"`
	UpdatedMs int64  `json:"updatedMs,omitempty"`
}

// Counts counts a run's agents by status.
type Counts struct {
	Running int `json:"running"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Stopped int `json:"stopped"`
}

// Stop marks a run that is still Running, and its running agents, as
// Stopped: it cannot go on (its session ended).
func (r *Run) Stop() {
	if r.Status != Running {
		return
	}
	r.Status = Stopped
	r.Tally()
}

// Tally fills in what follows from the agents: their counts and totals, the
// current phase, the phases only agents name, and UpdatedMs. Agents still
// running in a run that is not are Stopped.
func (r *Run) Tally() {
	r.Counts = Counts{}
	r.Tokens, r.ToolUses = 0, 0
	current, latest := "", ""
	for i := range r.Agents {
		a := &r.Agents[i]
		if a.Status == Running && r.Status != Running {
			a.Status = Stopped
		}
		switch a.Status {
		case Running:
			r.Counts.Running++
			current = a.Phase
		case Done:
			r.Counts.Done++
		case Failed:
			r.Counts.Failed++
		default:
			r.Counts.Stopped++
		}
		latest = a.Phase
		r.Tokens += a.Tokens
		r.ToolUses += a.ToolUses
		r.UpdatedMs = max(r.UpdatedMs, a.UpdatedMs)
		if a.Phase != "" && !slices.ContainsFunc(r.Phases, func(p Phase) bool { return p.Title == a.Phase }) {
			r.Phases = append(r.Phases, Phase{Title: a.Phase})
		}
	}
	if r.Counts.Running == 0 {
		current = latest
	}
	r.Phase = current
}
