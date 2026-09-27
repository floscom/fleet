package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/encoding/protojson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

func newLsCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list", "ps"},
		Short:   "List agents",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				agents, err := c.ListAgents(ctx, all)
				if err != nil {
					return err
				}
				sort.SliceStable(agents, func(i, j int) bool { return agents[i].CreatedAtMs < agents[j].CreatedAtMs })
				out := cmd.OutOrStdout()
				if asJSON {
					return writeAgentsJSON(out, agents)
				}
				if len(agents) == 0 {
					fmt.Fprintln(out, "no agents (start one with: fleet run <adapter> [path])")
					return nil
				}
				t := newTable(out, "ID", "NAME", "ADAPTER", "STATE", "PATH", "AGE")
				for _, a := range agents {
					t.row(a.Id, a.Name, a.Adapter, agentState(a), agentPath(a), age(a.CreatedAtMs))
				}
				t.flush()
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include exited and failed agents")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// agentPath shows where the agent runs, with its branch for worktrees and
// clones, and whether it is sandboxed.
func agentPath(a *fleetv1.Agent) string {
	p := a.Path
	if p == "" {
		p = a.Cwd
	}
	if a.CloneUrl != "" {
		p = a.CloneUrl
	}
	if a.Branch != "" {
		p += " [" + a.Branch + "]"
	}
	if a.Sandbox == fleetv1.Sandbox_SANDBOX_DOCKER {
		p += " (docker)"
	}
	return orDash(p)
}

func writeAgentsJSON(out interface{ Write([]byte) (int, error) }, agents []*fleetv1.Agent) error {
	opts := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	items := make([]json.RawMessage, len(agents))
	for i, a := range agents {
		b, err := opts.Marshal(a)
		if err != nil {
			return err
		}
		items[i] = b
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err = out.Write(buf.Bytes())
	return err
}

// parseRunPath splits a run target into root+rel or an absolute host path:
// "root:rel" (or "root:"), an absolute path, or a path relative to the cwd
// (local daemon only).
func parseRunPath(p string) (root, rel, abs string, err error) {
	if r, rest, ok := strings.Cut(p, ":"); ok && r != "" && !strings.ContainsRune(r, '/') {
		if rest == "" {
			rest = "."
		}
		return r, rest, "", nil
	}
	abs, err = hostPath(p)
	return "", "", abs, err
}

func newRunCmd() *cobra.Command {
	var (
		name, branch, prompt, clone, sandbox string
		pinned, worktree, attachTo, docker   bool
	)
	cmd := &cobra.Command{
		Use:   "run <adapter> [path] [-- extra args]",
		Short: "Start an agent (claude, codex, shell, ...) in a folder",
		Long: `Start an agent in tmux on the daemon host.

path is "root:rel/path", an absolute path, or (local daemon only) a path
relative to the current directory. Default ".". Inside a git repository the
agent gets its own worktree and branch unless --pinned is given.
With --clone, the daemon clones a repository instead (no path).

--docker runs the agent in a container that only sees its checkout
(the server needs Docker and the image from "fleet sandbox build").
Arguments after "--" are passed to the agent CLI.`,
		Example: `  fleet run claude
  fleet run codex code:api --prompt "fix the flaky test"
  fleet run claude ~/code/web --pinned --attach
  fleet run claude code:api --docker
  fleet run claude --clone owner/repo --docker --prompt "fix issue 42"
  fleet run claude . -- --model opus`,
		Args: cobra.ArbitraryArgs, // validated below; args after "--" are the agent's
		RunE: func(cmd *cobra.Command, args []string) error {
			var extra []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				args, extra = args[:dash], args[dash:]
			}
			if len(args) < 1 || len(args) > 2 {
				return errors.New("usage: fleet run <adapter> [path] [-- extra args]")
			}
			if pinned && worktree {
				return errors.New("--pinned and --worktree are mutually exclusive")
			}
			req := &fleetv1.RunAgentRequest{
				Adapter: args[0], Name: name, Branch: branch, Prompt: prompt, ExtraArgs: extra,
			}
			if clone != "" {
				if len(args) == 2 || pinned || worktree {
					return errors.New("--clone takes no path, --pinned or --worktree")
				}
				req.CloneUrl = clone
			} else {
				target := "."
				if len(args) == 2 {
					target = args[1]
				}
				var err error
				if req.Root, req.Path, req.AbsolutePath, err = parseRunPath(target); err != nil {
					return err
				}
			}
			switch {
			case pinned:
				req.Isolation = fleetv1.Isolation_ISOLATION_PINNED
			case worktree:
				req.Isolation = fleetv1.Isolation_ISOLATION_WORKTREE
			}
			if docker {
				if sandbox != "" && sandbox != "docker" {
					return errors.New("--docker and --sandbox " + sandbox + " contradict each other")
				}
				sandbox = "docker"
			}
			switch sandbox {
			case "":
			case "docker":
				req.Sandbox = fleetv1.Sandbox_SANDBOX_DOCKER
			case "none":
				req.Sandbox = fleetv1.Sandbox_SANDBOX_NONE
			default:
				return fmt.Errorf("unknown sandbox %q (docker or none)", sandbox)
			}
			if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				req.Cols, req.Rows = uint32(cols), uint32(rows)
			}
			ctx := cmd.Context()
			c, err := dial(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			a, err := c.RunAgent(ctx, req)
			if err != nil {
				return err
			}
			where := a.Cwd
			if a.Branch != "" {
				where += " on branch " + a.Branch
			}
			if a.Sandbox == fleetv1.Sandbox_SANDBOX_DOCKER {
				where += ", in docker"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "started %s (%s, %s) in %s\n", a.Name, a.Id, a.Adapter, where)
			if !attachTo {
				fmt.Fprintf(cmd.ErrOrStderr(), "attach with: fleet attach %s\n", a.Name)
				return nil
			}
			return attach(ctx, c, a.Id, false)
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "agent name (default: generated)")
	f.BoolVar(&pinned, "pinned", false, "run directly in the folder (no worktree)")
	f.BoolVar(&worktree, "worktree", false, "require a git worktree on a new branch")
	f.StringVar(&branch, "branch", "", "branch for the worktree or clone (default fleet/<name>)")
	f.StringVar(&clone, "clone", "", "clone this repository (URL or owner/repo) instead of using a folder")
	f.BoolVar(&docker, "docker", false, "run in a Docker sandbox (same as --sandbox docker)")
	f.StringVar(&sandbox, "sandbox", "", `"docker" or "none" (default: the server's [sandbox] default)`)
	f.StringVar(&prompt, "prompt", "", "initial prompt, if the adapter supports it")
	f.BoolVar(&attachTo, "attach", false, "attach to the agent's terminal right away")
	return cmd
}

func newKillCmd() *cobra.Command {
	var rmWorktree, force, forget bool
	cmd := &cobra.Command{
		Use:   "kill <agent>...",
		Short: "Stop agents (by id or name)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				var failed error
				for _, name := range args {
					r, err := c.KillAgent(ctx, &fleetv1.KillAgentRequest{Agent: name, RemoveWorktree: rmWorktree, Force: force, Forget: forget})
					if err != nil {
						failed = fmt.Errorf("%s: %w", name, err)
						fmt.Fprintln(cmd.ErrOrStderr(), "fleet:", failed)
						continue
					}
					a := r.GetAgent()
					verb := "killed"
					if forget {
						verb = "killed and forgot"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s)\n", verb, a.GetName(), a.GetId())
					if r.WorktreeKept && rmWorktree {
						what := "worktree"
						if a.GetIsolation() == fleetv1.Isolation_ISOLATION_CLONE {
							what = "clone"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "  %s kept: %s\n", what, r.WorktreeKeptReason)
					}
				}
				if failed != nil {
					return exitCode(1)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&rmWorktree, "rm-worktree", false, "also remove the agent's git worktree or clone (kept if it has uncommitted or unpushed work)")
	cmd.Flags().BoolVar(&force, "force", false, "remove the worktree even with uncommitted changes")
	cmd.Flags().BoolVar(&forget, "forget", false, "remove the agent from history too")
	return cmd
}

func newSendCmd() *cobra.Command {
	var noEnter bool
	cmd := &cobra.Command{
		Use:   "send <agent> <text...>",
		Short: "Type text into an agent's terminal and press Enter",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				return c.SendText(ctx, args[0], strings.Join(args[1:], " "), !noEnter)
			})
		},
	}
	cmd.Flags().BoolVar(&noEnter, "no-enter", false, "do not press Enter after the text")
	return cmd
}

func newWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Print agent state changes as they happen",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				events, err := c.Subscribe(ctx)
				if err != nil {
					return err
				}
				w := &watcher{out: cmd.OutOrStdout(), last: map[string]watched{}}
				for ev := range events {
					w.handle(ev)
				}
				if ctx.Err() != nil {
					return nil // interrupted
				}
				return c.Err()
			})
		},
	}
}

type watched struct {
	name   string
	state  fleetv1.AgentState
	detail string
}

type watcher struct {
	out  interface{ Write([]byte) (int, error) }
	last map[string]watched
}

func (w *watcher) printf(format string, a ...any) {
	fmt.Fprintf(w.out, time.Now().Format("15:04:05")+"  "+format+"\n", a...)
}

func (w *watcher) handle(ev *fleetv1.Event) {
	switch k := ev.Kind.(type) {
	case *fleetv1.Event_AgentUpserted:
		a := k.AgentUpserted
		prev, seen := w.last[a.Id]
		cur := watched{name: a.Name, state: a.State, detail: a.StateDetail}
		if seen && prev == cur {
			return
		}
		w.last[a.Id] = cur
		line := fmt.Sprintf("%-16s %-8s %-7s %s", a.Name, a.Id, a.Adapter, agentState(a))
		if a.StateDetail != "" {
			line += ": " + a.StateDetail
		}
		if !seen {
			line += "  " + agentPath(a)
		}
		w.printf("%s", line)
	case *fleetv1.Event_AgentRemoved:
		name := k.AgentRemoved
		if p, ok := w.last[k.AgentRemoved]; ok {
			name = fmt.Sprintf("%-16s %-8s", p.name, k.AgentRemoved)
		}
		delete(w.last, k.AgentRemoved)
		w.printf("%s removed", name)
	case *fleetv1.Event_SnapshotDone:
		if len(w.last) == 0 {
			w.printf("no agents; watching for changes (Ctrl-C to stop)")
		} else {
			w.printf("watching for changes (Ctrl-C to stop)")
		}
	case *fleetv1.Event_RootsChanged:
		names := make([]string, len(k.RootsChanged.Roots))
		for i, r := range k.RootsChanged.Roots {
			names[i] = r.Name
		}
		w.printf("roots: %s", orDash(strings.Join(names, ", ")))
	}
}
