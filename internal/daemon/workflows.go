package daemon

// The dashboard's workflow view: the workflow runs of an agent's session
// and the transcripts of their agents (see adapter.Workflower).

import (
	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/transcript"
	"fleet/internal/workflow"
)

// workflows returns an agent and the workflow runs of its session. Runs
// its session did not see end (it exited) are stopped.
func (m *manager) workflows(ref string) (*fleetv1.Agent, []workflow.Run, error) {
	s, err := m.session(ref)
	if err != nil {
		return nil, nil, err
	}
	w, ok := s.t.(adapter.Workflower)
	if !ok || s.path == "" {
		return s.agent, nil, nil
	}
	runs := w.Workflows(s.path)
	if !isLive(s.agent.State) {
		for i := range runs {
			runs[i].Stop()
		}
	}
	return s.agent, runs, nil
}

// workflowChat returns an agent, the transcript of agent sub of its
// workflow run run, and the parser for it.
func (m *manager) workflowChat(ref, run, sub string) (*fleetv1.Agent, string, transcript.Parser, error) {
	s, err := m.session(ref)
	if err != nil {
		return nil, "", nil, err
	}
	w, ok := s.t.(adapter.Workflower)
	if !ok || s.path == "" {
		return nil, "", nil, errf(codeNotFound, "agent %s has no workflows", s.agent.Id)
	}
	path, parse, ok := w.WorkflowTranscript(s.path, run, sub)
	if !ok || !inTranscriptDir(s.dir, path) {
		return nil, "", nil, errf(codeNotFound, "no agent %s in workflow %s", sub, run)
	}
	return s.agent, path, parse, nil
}
