package web

import (
	"context"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
)

// MergePull merges the agent's pull request with that URL.
func (s *fakeSource) MergePull(_ context.Context, req *fleetv1.MergePullRequestRequest) (*fleetv1.PullRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent != nil && req.Agent == s.agent.Id {
		for _, p := range s.agent.PullRequests {
			if p.Url == req.PullRequest {
				m := proto.Clone(p).(*fleetv1.PullRequest)
				m.State, m.MergedAtMs = fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED, 1
				return m, nil
			}
		}
	}
	return nil, &Error{Status: 404, Msg: "no pull request " + req.PullRequest}
}
