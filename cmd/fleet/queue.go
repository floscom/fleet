package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newQueueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Queue messages for an agent, sent one by one when it is done",
		Long: `Line up messages for a running agent. Once the agent finished its turn
and nothing runs in its background, the daemon types the next message
into its session, then waits for that turn to end, and so on. Stopping a
turn from fleet (fleet interrupt, Stop in the dashboard) pauses the
queue; fleet queue resume goes on.`,
		Example: `  fleet queue add claude-api-1 now write the tests for it
  fleet queue ls claude-api-1
  fleet queue rm claude-api-1 2`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "add <agent> <text...>",
			Short: "Queue a message (at most 50, of up to 10000 characters)",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(cmd, func(ctx context.Context, c *client.Client) error {
					a, err := c.EditQueue(ctx, &fleetv1.EditQueueRequest{Agent: args[0],
						Op: &fleetv1.EditQueueRequest_Add{Add: strings.Join(args[1:], " ")}})
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "queued for %s as #%d\n", a.Name, len(a.Queue))
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "ls <agent>",
			Short: "List the queued messages, next first",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(cmd, func(ctx context.Context, c *client.Client) error {
					a, err := findAgent(ctx, c, args[0])
					if err != nil {
						return err
					}
					printQueue(cmd, a)
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "rm <agent> <#|id>...",
			Short: "Remove queued messages, by their number in fleet queue ls or id",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(cmd, func(ctx context.Context, c *client.Client) error {
					a, err := findAgent(ctx, c, args[0])
					if err != nil {
						return err
					}
					// Numbers count in the queue as it is now, before any removal.
					var ids []string
					for _, ref := range args[1:] {
						id := ref
						if n, err := strconv.Atoi(ref); err == nil {
							if n < 1 || n > len(a.Queue) {
								return fmt.Errorf("%s has %d queued messages, no #%d", a.Name, len(a.Queue), n)
							}
							id = a.Queue[n-1].Id
						}
						ids = append(ids, id)
					}
					for _, id := range ids {
						if a, err = c.EditQueue(ctx, &fleetv1.EditQueueRequest{Agent: a.Id, Op: &fleetv1.EditQueueRequest_Remove{Remove: id}}); err != nil {
							return err
						}
					}
					printQueue(cmd, a)
					return nil
				})
			},
		},
		queuePauseCmd("pause", "Hold the queue: no message is sent until resumed", true),
		queuePauseCmd("resume", "Go on sending queued messages", false),
	)
	return cmd
}

func queuePauseCmd(use, short string, paused bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <agent>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				a, err := c.EditQueue(ctx, &fleetv1.EditQueueRequest{Agent: args[0], Op: &fleetv1.EditQueueRequest_Paused{Paused: paused}})
				if err != nil {
					return err
				}
				printQueue(cmd, a)
				return nil
			})
		},
	}
}

func printQueue(cmd *cobra.Command, a *fleetv1.Agent) {
	out := cmd.OutOrStdout()
	if len(a.Queue) == 0 {
		fmt.Fprintf(out, "nothing is queued for %s\n", a.Name)
		return
	}
	if a.QueuePaused {
		fmt.Fprintf(out, "the queue of %s is paused (fleet queue resume %s)\n", a.Name, a.Name)
	}
	for i, q := range a.Queue {
		fmt.Fprintf(out, "%d  %s  %s\n", i+1, q.Id, oneLine(q.Text))
	}
}
