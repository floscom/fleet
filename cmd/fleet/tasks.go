package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newTasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Show the task trackers fleet takes work from",
		Long: `Task trackers ([[tasks]] in config.toml) give fleet work through their
account's queue (Rome: a pull webhook, see fleet tasks setup). fleet takes
the next batch only while fewer than max_agents of its agents are busy.
The batch's tickets in a project mapped to a folder start one agent there
that works them in order, named after the first (rome-42, rome-42-and-2).
fleet tells it how to work the tracker, gives it the tracker's MCP server
and token, passes it the news on its tickets (answers to its questions,
mentions, decisions) through its queue, and sends the tracker heartbeats
while it works. The batch is acknowledged once its agents are done with
their turn; until then it is leased, and handed out again if fleet goes
away. Once all of an agent's tickets are done and it has been idle for
close_after (default 1h, "off" never), fleet archives its session.

  [[tasks]]
  kind = "rome"
  url = "https://rome.example.com"
  token_file = "~/.config/rome/fleet-token"   # the agent account's token
  # webhook = "whk_…" when the account has several pull webhooks
  # close_after = "1h"  # archive an agent once its tickets are done and it idled this long; "off"
  # adapter, model, effort, isolation, sandbox, args, max_agents, instructions

  [[tasks.project]]
  key = "ROME"
  path = "/srv/rome"          # inside a root; or root = "name" (+ path)
  # adapter, model, effort, isolation, sandbox, instructions

Change it with fleet tasks add, rm, map, unmap and set: changes apply at
once. After editing config.toml by hand, restart the daemon.

A tracker with agent setup (Rome) also asks its people for the folder of
each project the account is a member of: a project set up there is worked
in that folder (inside a root) unless config.toml maps it, and fleet tells
the tracker per project whether it can work it. Tickets assigned while a
project was not set up start once it is.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				srcs, err := c.ListTaskSources(ctx)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(srcs) == 0 {
					fmt.Fprintln(out, "no task trackers are set up: add one with fleet tasks add (see fleet tasks --help)")
					return nil
				}
				for i, s := range srcs {
					if i > 0 {
						fmt.Fprintln(out)
					}
					status := "polling"
					switch {
					case s.Disabled:
						status = "disabled"
					case s.Error != "":
						status = "error: " + s.Error
					case s.LastPollMs == 0:
						status = "connecting"
					}
					account := s.Account
					if account == "" {
						account = "?"
					} else {
						account = "@" + account
					}
					fmt.Fprintf(out, "%s (%s) %s as %s: %s\n", s.Name, s.Kind, s.Url, account, status)
					if s.Webhook != "" {
						fmt.Fprintf(out, "  queue: %s, %d batch(es) in work\n", s.Webhook, s.InFlight)
					}
					fmt.Fprintf(out, "  agents working: %d of %d", s.Working, s.MaxAgents)
					if len(s.Waiting) > 0 {
						fmt.Fprintf(out, ", waiting: %s", strings.Join(s.Waiting, " "))
					}
					if s.LastPollMs > 0 {
						fmt.Fprintf(out, ", last answer %s ago", age(s.LastPollMs))
					}
					fmt.Fprintln(out)
					if s.CloseAfterMs > 0 {
						fmt.Fprintf(out, "  agents of done tickets close after %s idle\n", closeAfter(time.Duration(s.CloseAfterMs)*time.Millisecond))
					} else {
						fmt.Fprintln(out, "  agents of done tickets stay open (close_after off)")
					}
					if len(s.Projects) == 0 {
						fmt.Fprintln(out, "  no projects mapped: add [[tasks.project]] entries")
						continue
					}
					t := newTable(out, "  PROJECT", "ADAPTER", "FOLDER")
					for _, p := range s.Projects {
						dir := p.Path
						if dir == "" {
							dir = "-"
						}
						switch {
						case p.Error != "":
							dir += " (" + p.Error + ")"
						case p.Tracker:
							dir += " (set up in " + s.Kind + ")"
						}
						t.row("  "+p.Key, p.Adapter, dir)
					}
					t.flush()
				}
				return nil
			})
		},
	}
	var source string
	start := &cobra.Command{
		Use:   "start <ticket>",
		Short: "Start an agent on a ticket now",
		Long: `Start an agent on a ticket now, as if it had just been assigned to the
tracker account (max_agents does not hold it back). If an agent works the
ticket already, prints that one. The ticket's project must be mapped to a
folder.`,
		Example: `  fleet tasks start ROME-42`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				a, err := c.StartTask(ctx, source, args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s works %s in %s\n", a.Name, strings.Join(a.GetTask().GetKeys(), " "), a.Cwd)
				return nil
			})
		},
	}
	start.Flags().StringVar(&source, "source", "", "the [[tasks]] source, when there are several")
	var quiet, maxWait uint32
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Create the tracker account's queue fleet takes work from",
		Long: `Create the queue fleet takes work from, unless the account has one:
for Rome a pull webhook of the account with collected delivery, carrying
what is addressed to it (assignments, mentions, answers to its asks,
withdrawn asks, approval decisions) in the projects mapped under
[[tasks.project]]. A batch goes out --quiet minutes after its newest
event, at the latest --max minutes after its oldest, at once on a reply;
0 takes Rome's defaults (10 and 60). Change it later in Rome. If the
account has a pull webhook already, its projects are brought in line with
the mapped ones (unless it carries every project).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				r, err := c.SetupTaskSource(ctx, &fleetv1.SetupTaskSourceRequest{Source: source, QuietMinutes: quiet, MaxMinutes: maxWait})
				if err != nil {
					return err
				}
				if r.Created {
					fmt.Fprintf(cmd.OutOrStdout(), "created pull webhook %s\n", r.Webhook)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "the account already has pull webhook %s\n", r.Webhook)
				}
				if r.Note != "" {
					fmt.Fprintln(cmd.OutOrStdout(), r.Note)
				}
				return nil
			})
		},
	}
	setup.Flags().StringVar(&source, "source", "", "the [[tasks]] source, when there are several")
	setup.Flags().Uint32Var(&quiet, "quiet", 0, "minutes without a new event before a batch goes out (0: Rome's default)")
	setup.Flags().Uint32Var(&maxWait, "max", 0, "minutes a batch waits at most (0: Rome's default)")
	cmd.AddCommand(start, setup, newTasksAddCmd(), newTasksRmCmd(), newTasksMapCmd(), newTasksUnmapCmd(), newTasksSetCmd())
	return cmd
}

// closeAfter renders a close_after duration as it is written: 1h, 1h30m,
// 45m, 90s.
func closeAfter(d time.Duration) string {
	s := d.String()
	if d >= time.Minute && d%time.Minute == 0 {
		s = strings.TrimSuffix(s, "0s")
		if d%time.Hour == 0 {
			s = strings.TrimSuffix(s, "0m")
		}
	}
	return s
}
