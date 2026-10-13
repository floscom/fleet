package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newNoteCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "note <agent> [text...]",
		Short: "Show, set or clear the note on an agent",
		Long: `Leave a note on an agent, e.g. why it must stay open. The dashboard
shows it on the agent's row and in its session, fleet ls in a NOTE
column. Without text, prints the note; --clear removes it. At most 500
characters.`,
		Example: `  fleet note claude-api-1 customer is testing the preview, keep it open
  fleet note claude-api-1
  fleet note claude-api-1 --clear`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clear && len(args) > 1 {
				return fmt.Errorf("--clear takes no text")
			}
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				out := cmd.OutOrStdout()
				if len(args) == 1 && !clear {
					a, err := findAgent(ctx, c, args[0])
					if err != nil {
						return err
					}
					if a.Note == "" {
						fmt.Fprintf(out, "%s has no note\n", a.Name)
					} else {
						fmt.Fprintln(out, a.Note)
					}
					return nil
				}
				a, err := c.SetNote(ctx, args[0], strings.Join(args[1:], " "))
				if err != nil {
					return err
				}
				if a.Note == "" {
					fmt.Fprintf(out, "cleared the note on %s\n", a.Name)
				} else {
					fmt.Fprintf(out, "noted on %s: %s\n", a.Name, a.Note)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&clear, "clear", "d", false, "remove the note")
	return cmd
}

// findAgent finds an agent by id or name as the daemon does: a live one
// wins over finished ones of the same name, the newest of those over the
// others.
func findAgent(ctx context.Context, c *client.Client, ref string) (*fleetv1.Agent, error) {
	agents, err := c.ListAgents(ctx, true)
	if err != nil {
		return nil, err
	}
	var best *fleetv1.Agent
	for _, a := range agents {
		switch {
		case a.Id == ref:
			return a, nil
		case a.Name != ref:
		case best == nil || isLiveAgent(a) && !isLiveAgent(best) ||
			isLiveAgent(a) == isLiveAgent(best) && a.CreatedAtMs > best.CreatedAtMs:
			best = a
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no agent %q", ref)
	}
	return best, nil
}

func isLiveAgent(a *fleetv1.Agent) bool {
	return a.State >= fleetv1.AgentState_AGENT_STATE_STARTING && a.State <= fleetv1.AgentState_AGENT_STATE_NEEDS_INPUT
}

// oneLine joins the lines of s with spaces, for a table cell.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
