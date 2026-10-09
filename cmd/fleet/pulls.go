package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newPullsCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:     "prs [agent]",
		Aliases: []string{"pulls"},
		Short:   "List the GitHub pull requests agents created",
		Long: `List the GitHub pull requests agents created (with gh pr create),
open ones unless --all, as the daemon host's gh last saw them.
Merge one with: fleet merge <agent> [pr]`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				agents, err := c.ListAgents(ctx, true)
				if err != nil {
					return err
				}
				sort.SliceStable(agents, func(i, j int) bool { return agents[i].CreatedAtMs < agents[j].CreatedAtMs })
				type row struct {
					a *fleetv1.Agent
					p *fleetv1.PullRequest
				}
				var rows []row
				found := len(args) == 0
				for _, a := range agents {
					if len(args) == 1 {
						if a.Id != args[0] && a.Name != args[0] {
							continue
						}
						found = true
					}
					for _, p := range a.PullRequests {
						if all || pullOpen(p) {
							rows = append(rows, row{a, p})
						}
					}
				}
				if !found {
					return fmt.Errorf("no agent %q", args[0])
				}
				out := cmd.OutOrStdout()
				if asJSON {
					opts := protojson.MarshalOptions{UseProtoNames: true}
					items := make([]json.RawMessage, 0, len(rows))
					for _, r := range rows {
						b, err := opts.Marshal(r.p)
						if err != nil {
							return err
						}
						items = append(items, json.RawMessage(fmt.Sprintf(`{"agent":%q,"agent_name":%q,"pull_request":%s}`, r.a.Id, r.a.Name, b)))
					}
					b, err := json.MarshalIndent(items, "", "  ")
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(out, "%s\n", b)
					return err
				}
				if len(rows) == 0 {
					if all {
						fmt.Fprintln(out, "no pull requests")
					} else {
						fmt.Fprintln(out, "no open pull requests (--all shows merged and closed ones)")
					}
					return nil
				}
				t := newTable(out, "AGENT", "PR", "STATE", "CHECKS", "CHANGES", "TITLE", "URL")
				for _, r := range rows {
					p := r.p
					changes := "-"
					if p.ChangedFiles > 0 || p.Additions > 0 || p.Deletions > 0 {
						changes = fmt.Sprintf("+%d -%d", p.Additions, p.Deletions)
					}
					t.row(r.a.Name, "#"+strconv.Itoa(int(p.Number)), pullState(p), checksName(p.Checks),
						changes, orDash(truncate(p.Title, 50)), p.Url)
				}
				t.flush()
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include merged and closed pull requests")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newMergeCmd() *cobra.Command {
	var squash, merge, rebase, deleteBranch bool
	cmd := &cobra.Command{
		Use:   "merge <agent> [pr]",
		Short: "Merge a pull request an agent created",
		Long: `Merge a GitHub pull request an agent created, with the daemon host's gh.
pr is its number or URL; without it, the agent's only open pull request.
A draft is marked ready for review first. Without a method flag the first
one the repository allows is used: squash, merge, rebase.`,
		Example: `  fleet merge claude-api-1
  fleet merge claude-api-1 42 --rebase --delete-branch`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &fleetv1.MergePullRequestRequest{Agent: args[0], DeleteBranch: deleteBranch}
			if len(args) == 2 {
				req.PullRequest = args[1]
			}
			n := 0
			for flag, m := range map[*bool]fleetv1.MergeMethod{
				&squash: fleetv1.MergeMethod_MERGE_METHOD_SQUASH,
				&merge:  fleetv1.MergeMethod_MERGE_METHOD_MERGE,
				&rebase: fleetv1.MergeMethod_MERGE_METHOD_REBASE,
			} {
				if *flag {
					req.Method = m
					n++
				}
			}
			if n > 1 {
				return errors.New("choose one of --squash, --merge, --rebase")
			}
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				p, err := c.MergePullRequest(ctx, req)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s#%d: %s\n  %s\n", pullState(p), p.Repo, p.Number, p.Title, p.Url)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&squash, "squash", false, "squash the commits into one")
	cmd.Flags().BoolVar(&merge, "merge", false, "create a merge commit")
	cmd.Flags().BoolVar(&rebase, "rebase", false, "rebase the commits onto the base branch")
	cmd.Flags().BoolVarP(&deleteBranch, "delete-branch", "d", false, "delete the branch on GitHub after merging")
	return cmd
}

// pullOpen reports whether p is open, or not looked up yet.
func pullOpen(p *fleetv1.PullRequest) bool {
	switch p.State {
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED, fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED:
		return false
	}
	return true
}

// pullState says whether p is open (and how far from mergeable), merged or closed.
func pullState(p *fleetv1.PullRequest) string {
	switch p.State {
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_MERGED:
		return "merged"
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_CLOSED:
		return "closed"
	case fleetv1.PullRequestState_PULL_REQUEST_STATE_UNSPECIFIED:
		if p.Detail != "" {
			return "unknown"
		}
		return "checking"
	}
	if p.Draft {
		return "draft"
	}
	switch p.MergeState {
	case "dirty":
		return "conflicts"
	case "behind":
		return "behind"
	case "blocked":
		return "blocked"
	}
	return "open"
}

func checksName(c fleetv1.ChecksState) string {
	switch c {
	case fleetv1.ChecksState_CHECKS_STATE_PENDING:
		return "pending"
	case fleetv1.ChecksState_CHECKS_STATE_PASSING:
		return "passing"
	case fleetv1.ChecksState_CHECKS_STATE_FAILING:
		return "failing"
	}
	return "-"
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
