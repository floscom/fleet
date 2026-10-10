package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newGitCmd() *cobra.Command {
	var rebase, merge, base, force, asJSON bool
	cmd := &cobra.Command{
		Use:   "git <agent> [status|fetch|pull|push]",
		Short: "Show, fetch, pull or push an agent's checkout",
		Long: `Show the git state of an agent's checkout (its worktree, clone or
pinned directory), or fetch, pull or push it with git on the daemon host,
using that user's git credentials.

pull fetches, then brings in the branch's upstream (with --base, or when
the branch has none: the remote's default branch, e.g. origin/main).
It fast-forwards only unless --rebase or --merge, refuses while the agent
is working, and aborts a merge or rebase that conflicts.

push pushes the branch under the same name to its remote and makes that
its upstream. --force pushes with --force-with-lease.`,
		Example: `  fleet git claude-api-1
  fleet git claude-api-1 pull --base --rebase
  fleet git claude-api-1 push`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &fleetv1.GitRequest{Agent: args[0], FromBase: base, Force: force}
			action := "status"
			if len(args) == 2 {
				action = args[1]
			}
			switch action {
			case "status":
				req.Action = fleetv1.GitAction_GIT_ACTION_STATUS
			case "fetch":
				req.Action = fleetv1.GitAction_GIT_ACTION_FETCH
			case "pull":
				req.Action = fleetv1.GitAction_GIT_ACTION_PULL
			case "push":
				req.Action = fleetv1.GitAction_GIT_ACTION_PUSH
			default:
				return fmt.Errorf("unknown action %q: status, fetch, pull or push", action)
			}
			switch {
			case rebase && merge:
				return errors.New("choose one of --rebase, --merge")
			case rebase:
				req.PullMode = fleetv1.PullMode_PULL_MODE_REBASE
			case merge:
				req.PullMode = fleetv1.PullMode_PULL_MODE_MERGE
			}
			if (rebase || merge || base) && action != "pull" {
				return errors.New("--rebase, --merge and --base are for pull")
			}
			if force && action != "push" {
				return errors.New("--force is for push")
			}
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				r, err := c.Git(ctx, req)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if asJSON {
					b, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true}.Marshal(r)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(out, "%s\n", b)
					return err
				}
				if r.Output != "" {
					fmt.Fprintln(out, r.Output)
				}
				printGitStatus(out, r.Status)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&rebase, "rebase", false, "pull: rebase the branch's commits onto the other side")
	cmd.Flags().BoolVar(&merge, "merge", false, "pull: merge the other side in")
	cmd.Flags().BoolVar(&base, "base", false, "pull: from the remote's default branch, not the upstream")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "push: --force-with-lease")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// printGitStatus prints a checkout's state in a few lines.
func printGitStatus(w io.Writer, st *fleetv1.GitStatus) {
	if st == nil {
		fmt.Fprintln(w, "not in a git repository")
		return
	}
	branch := st.Branch
	if branch == "" {
		branch = "(detached)"
	}
	line := branch
	if st.Upstream != "" {
		line += " → " + st.Upstream + gitCounts(st.Ahead, st.Behind)
	} else {
		line += " (not pushed)"
	}
	fmt.Fprintln(w, line)
	if st.Base != "" {
		fmt.Fprintf(w, "  vs %s%s\n", st.Base, gitCounts(st.BaseAhead, st.BaseBehind))
	}
	if st.Head != "" {
		fmt.Fprintf(w, "  %s %s (%s ago)\n", st.Head, truncate(st.HeadSubject, 60), age(st.HeadTimeMs))
	}
	var notes []string
	if st.Changed > 0 {
		notes = append(notes, fmt.Sprintf("%d uncommitted", st.Changed))
	}
	if st.Conflicts > 0 {
		notes = append(notes, fmt.Sprintf("%d conflicted", st.Conflicts))
	}
	if st.Operation != "" {
		notes = append(notes, st.Operation+" in progress")
	}
	if st.FetchedAtMs > 0 {
		notes = append(notes, "fetched "+age(st.FetchedAtMs)+" ago")
	}
	if len(notes) > 0 {
		fmt.Fprintln(w, "  "+strings.Join(notes, " · "))
	}
	fmt.Fprintln(w, "  "+st.Dir)
	if st.Detail != "" {
		fmt.Fprintln(w, "  ! "+st.Detail)
	}
}

// gitCounts is " ↑a ↓b", or " (up to date)".
func gitCounts(ahead, behind int32) string {
	if ahead == 0 && behind == 0 {
		return " (up to date)"
	}
	s := ""
	if ahead > 0 {
		s += fmt.Sprintf(" ↑%d", ahead)
	}
	if behind > 0 {
		s += fmt.Sprintf(" ↓%d", behind)
	}
	return s
}
