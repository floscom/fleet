package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

// hostPath makes a user-supplied path suitable for the daemon: relative paths
// are resolved against the cwd for the local daemon and rejected for remote
// ones, where the cwd means nothing.
func hostPath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if !client.IsLocal(host) {
		return "", fmt.Errorf("%q is relative; use an absolute path on %s (or root:path)", p, host)
	}
	return filepath.Abs(p)
}

func newRootsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roots",
		Short: "List the folders agents may run in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				roots, err := c.ListRoots(ctx)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(roots) == 0 {
					fmt.Fprintln(out, "no roots yet (add one with: fleet roots add <path>)")
					return nil
				}
				t := newTable(out, "NAME", "PATH", "ADAPTERS", "TRUST")
				for _, r := range roots {
					t.row(r.Name, r.Path, adapterList(r.Adapters), yesNo(r.Trust))
				}
				t.flush()
				return nil
			})
		},
	}

	var name string
	var adapters []string
	var trust bool
	add := &cobra.Command{
		Use:   "add <path>",
		Short: "Allow agents to run inside a folder",
		Long: `Allow agents to run inside a folder.

With --trust, agents started here skip the agent CLI's own "Do you trust
this folder?" prompt: before launching, fleet marks the agent's git
repository (or, outside git, its folder) as trusted for Claude Code and
Codex, as if you had answered yes. A trusted folder may run its own hooks,
MCP servers and settings, so only use it for code you trust.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := hostPath(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				r, err := c.AddRoot(ctx, &fleetv1.AddRootRequest{Path: p, Name: name, Adapters: adapters, Trust: trust})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "added root %s -> %s (adapters: %s, trust: %s)\n", r.Name, r.Path, adapterList(r.Adapters), yesNo(r.Trust))
				return nil
			})
		},
	}
	add.Flags().StringVar(&name, "name", "", "root name (default: folder name)")
	add.Flags().StringSliceVar(&adapters, "adapters", nil, "adapters allowed here, e.g. claude,codex (default: all)")
	add.Flags().BoolVar(&trust, "trust", false, "pre-answer the agent CLIs' folder trust prompt for agents started here")

	rm := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a root (running agents are not affected)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				if err := c.RemoveRoot(ctx, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed root %s\n", args[0])
				return nil
			})
		},
	}
	cmd.AddCommand(add, rm)
	return cmd
}

// addLocalRoots makes each of paths (absolute, symlink-resolved) a root of
// the local daemon unless it already is one (`fleet start --root`).
func addLocalRoots(ctx context.Context, out io.Writer, paths []string, trust bool) error {
	if len(paths) == 0 {
		return nil
	}
	c, err := client.DialLocal(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	roots, err := c.ListRoots(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		i := slices.IndexFunc(roots, func(r *fleetv1.Root) bool { return r.Path == p })
		if i >= 0 {
			r := roots[i]
			fmt.Fprintf(out, "%s is already root %s (trust: %s)\n", p, r.Name, yesNo(r.Trust))
			if trust && !r.Trust {
				fmt.Fprintf(out, "  --trust only applies to new roots; to trust it: fleet roots rm %s && fleet roots add %s --trust\n", r.Name, p)
			}
			continue
		}
		r, err := c.AddRoot(ctx, &fleetv1.AddRootRequest{Path: p, Trust: trust})
		if err != nil {
			return err
		}
		roots = append(roots, r)
		fmt.Fprintf(out, "added root %s -> %s (adapters: %s, trust: %s)\n", r.Name, r.Path, adapterList(r.Adapters), yesNo(r.Trust))
	}
	return nil
}

func adapterList(a []string) string {
	if len(a) == 0 {
		return "all"
	}
	return strings.Join(a, ",")
}

func newBrowseCmd() *cobra.Command {
	var hidden bool
	cmd := &cobra.Command{
		Use:   "browse [root[/path]]",
		Short: "List folders inside a root",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				out := cmd.OutOrStdout()
				if len(args) == 0 {
					roots, err := c.ListRoots(ctx)
					if err != nil {
						return err
					}
					for _, r := range roots {
						fmt.Fprintf(out, "%s/\t%s\n", r.Name, r.Path)
					}
					return nil
				}
				root, rel, _ := strings.Cut(strings.TrimPrefix(args[0], "/"), "/")
				r, err := c.Browse(ctx, &fleetv1.BrowseRequest{Root: root, Path: rel, IncludeHidden: hidden})
				if err != nil {
					return err
				}
				for _, e := range r.Entries {
					var notes []string
					if e.IsGitRepo {
						notes = append(notes, "git")
					}
					if e.AgentCount > 0 {
						notes = append(notes, fmt.Sprintf("%d agent(s)", e.AgentCount))
					}
					line := e.Name + "/"
					if len(notes) > 0 {
						line += "  (" + strings.Join(notes, ", ") + ")"
					}
					fmt.Fprintln(out, line)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&hidden, "all", "a", false, "include hidden folders")
	return cmd
}

func newAdaptersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "adapters",
		Short: "List agent adapters and whether their CLI is installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				list, err := c.ListAdapters(ctx)
				if err != nil {
					return err
				}
				t := newTable(cmd.OutOrStdout(), "ID", "NAME", "AVAILABLE", "VERSION", "PATH", "FEATURES")
				for _, a := range list {
					avail := yesNo(a.Available)
					if !a.Available && a.UnavailableReason != "" {
						avail = "no: " + a.UnavailableReason
					}
					t.row(a.Id, a.DisplayName, avail, orDash(a.Version), orDash(a.BinaryPath), features(a.Capabilities))
				}
				t.flush()
				// What `fleet run --model/--effort` take.
				var choices [][]string
				for _, a := range list {
					if len(a.Models) == 0 && len(a.Efforts) == 0 {
						continue
					}
					var ms, es []string
					for _, m := range a.Models {
						ms = append(ms, m.Id)
					}
					for _, e := range a.Efforts {
						es = append(es, e.Id)
					}
					choices = append(choices, []string{a.Id, orDash(strings.Join(ms, ",")), orDash(strings.Join(es, ","))})
				}
				if len(choices) > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
					t := newTable(cmd.OutOrStdout(), "ID", "--MODEL", "--EFFORT")
					for _, c := range choices {
						t.row(c...)
					}
					t.flush()
				}
				return nil
			})
		},
	}
}

func features(c *fleetv1.AdapterCapabilities) string {
	var f []string
	if c.GetActivityState() {
		f = append(f, "status")
	}
	if c.GetInitialPrompt() {
		f = append(f, "prompt")
	}
	if c.GetResume() {
		f = append(f, "resume")
	}
	return orDash(strings.Join(f, ","))
}
