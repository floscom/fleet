package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
	"fleet/internal/config"
)

// editTasks sends an edit and prints what came of it.
func editTasks(cmd *cobra.Command, req *fleetv1.EditTasksRequest, done func(srcs []*fleetv1.TaskSource) string) error {
	return withClient(cmd, func(ctx context.Context, c *client.Client) error {
		r, err := c.EditTasks(ctx, req)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, done(r.Sources))
		if r.Note != "" {
			fmt.Fprintln(out, r.Note)
		}
		return nil
	})
}

// tokenPath is a token file as the daemon reads it: "~/..." is kept for
// the daemon to expand in its own home.
func tokenPath(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		return p, nil
	}
	return hostPath(p)
}

func newTasksAddCmd() *cobra.Command {
	var spec fleetv1.TaskSourceSpec
	cmd := &cobra.Command{
		Use:   "add [name] --url URL (--token-file FILE | --token-env VAR)",
		Short: "Add a task tracker to take work from",
		Long: `Add a [[tasks]] entry and start taking work from it, without a restart.
The token is the tracker account's (for Rome a service account or private
agent, never your own): give a file holding it, or an environment
variable of the daemon. The token itself is never sent or stored by
fleet. Then map projects with fleet tasks map and create the account's
queue with fleet tasks setup.`,
		Example: `  fleet tasks add --url https://rome.example.com --token-file ~/.config/rome/fleet-token
  fleet tasks add staging --url https://rome-staging.example.com --token-env ROME_STAGING_TOKEN`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				spec.Name = args[0]
			}
			if spec.Url == "" {
				return fmt.Errorf("--url is required")
			}
			if (spec.TokenFile == "") == (spec.TokenEnv == "") {
				return fmt.Errorf("give one of --token-file and --token-env")
			}
			if spec.TokenFile != "" {
				p, err := tokenPath(spec.TokenFile)
				if err != nil {
					return err
				}
				spec.TokenFile = p
			}
			return editTasks(cmd, &fleetv1.EditTasksRequest{Op: &fleetv1.EditTasksRequest_AddSource{AddSource: &spec}}, func([]*fleetv1.TaskSource) string {
				return fmt.Sprintf("added %s; map its projects with fleet tasks map", nameOr(spec.Name, nameOr(spec.Kind, "rome")))
			})
		},
	}
	cmd.Flags().StringVar(&spec.Kind, "kind", "rome", "the tracker: "+strings.Join(config.TaskKinds, ", "))
	cmd.Flags().StringVar(&spec.Url, "url", "", "the tracker's base URL")
	cmd.Flags().StringVar(&spec.TokenFile, "token-file", "", "a file holding the account's token")
	cmd.Flags().StringVar(&spec.TokenEnv, "token-env", "", "an environment variable of the daemon holding the token")
	cmd.Flags().StringVar(&spec.Webhook, "webhook", "", `the queue (Rome pull webhook "whk_…"), when the account has several`)
	return cmd
}

func nameOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func newTasksRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Stop taking work from a task tracker and remove it",
		Long: `Remove a [[tasks]] entry. Its agents keep running; batches it holds are
handed out again by the tracker once their lease runs out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return editTasks(cmd, &fleetv1.EditTasksRequest{Op: &fleetv1.EditTasksRequest_RemoveSource{RemoveSource: args[0]}}, func([]*fleetv1.TaskSource) string {
				return "removed " + args[0]
			})
		},
	}
}

func newTasksMapCmd() *cobra.Command {
	var (
		source, root, adapter, model, effort, isolation, sandbox, instructions, argLine string
	)
	cmd := &cobra.Command{
		Use:   "map <project> [folder] [flags]",
		Short: "Map a tracker project to the folder its tickets are worked in",
		Long: `Map a project of the tracker (its key, e.g. ROME) to a folder, or change
its mapping: the flags you give replace those settings, the others stay
("" clears one). The folder must be inside a root: an absolute path, a
path relative to the current folder (for the local daemon), or with
--root a path inside that root. The change applies at once, and the
account's queue (Rome pull webhook) is brought in line with the mapped
projects, unless it carries every project.`,
		Example: `  fleet tasks map ROME /mnt/data/rome
  fleet tasks map WEB klixpert-web --root code --adapter codex --model gpt-6
  fleet tasks map WEB --instructions "Use pnpm. Run the e2e tests before you finish."
  fleet tasks map OPS --args "--permission-mode acceptEdits"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m := &fleetv1.TaskProjectEdit{Source: source, Key: args[0]}
			f := cmd.Flags()
			str := func(name string, v string) *string {
				if f.Changed(name) {
					return &v
				}
				return nil
			}
			if len(args) == 2 {
				p := args[1]
				if root == "" {
					var err error
					if p, err = hostPath(p); err != nil {
						return err
					}
				}
				m.Path = &p
			}
			m.Root = str("root", root)
			m.Adapter, m.Model, m.Effort = str("adapter", adapter), str("model", model), str("effort", effort)
			m.Isolation, m.Sandbox, m.Instructions = str("isolation", isolation), str("sandbox", sandbox), str("instructions", instructions)
			if f.Changed("args") {
				m.Args, m.SetArgs = strings.Fields(argLine), true
			}
			return editTasks(cmd, &fleetv1.EditTasksRequest{Op: &fleetv1.EditTasksRequest_MapProject{MapProject: m}}, func(srcs []*fleetv1.TaskSource) string {
				for _, s := range srcs {
					for _, p := range s.Projects {
						if strings.EqualFold(p.Key, args[0]) && (source == "" || s.Name == source) {
							return fmt.Sprintf("%s → %s (%s)", p.Key, p.Path, p.Adapter)
						}
					}
				}
				return "mapped " + strings.ToUpper(args[0])
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&source, "source", "", "the [[tasks]] source, when there are several")
	f.StringVar(&root, "root", "", "a root's name: the folder is a path inside it")
	f.StringVar(&adapter, "adapter", "", "the agent CLI (default: the source's, else claude)")
	f.StringVar(&model, "model", "", "the model")
	f.StringVar(&effort, "effort", "", "the reasoning effort")
	f.StringVar(&isolation, "isolation", "", `"worktree" or "pinned"`)
	f.StringVar(&sandbox, "sandbox", "", `"none" or "docker"`)
	f.StringVar(&instructions, "instructions", "", "what this project's agents are told, after the source's")
	f.StringVar(&argLine, "args", "", "extra arguments for the agent CLI, space-separated")
	return cmd
}

func newTasksUnmapCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "unmap <project>",
		Short: "Stop taking tickets of a tracker project",
		Long: `Remove a project's mapping. Its tickets are ignored from then on; agents
working them keep running. The account's queue drops the project too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &fleetv1.EditTasksRequest{Op: &fleetv1.EditTasksRequest_UnmapProject{UnmapProject: &fleetv1.TaskProjectRef{Source: source, Key: args[0]}}}
			return editTasks(cmd, req, func([]*fleetv1.TaskSource) string { return "unmapped " + strings.ToUpper(args[0]) })
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "the [[tasks]] source, when there are several")
	return cmd
}

func newTasksSetCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "set <option> [value...]",
		Short: "Change a setting of a task tracker",
		Long: `Change a [[tasks]] setting; without a value it is unset (back to its
default). Options: ` + strings.Join(config.TaskOptions, ", ") + `.
url, token_file, token_env, webhook and disabled restart the source; the
others apply to the next agents it starts. args takes several values (put
-- before ones that start with -); the agent settings are the defaults of
every project, which can override them (fleet tasks map).`,
		Example: `  fleet tasks set max_agents 5
  fleet tasks set model sonnet
  fleet tasks set instructions "Open a pull request when you are done."
  fleet tasks set args -- --permission-mode acceptEdits
  fleet tasks set disabled true
  fleet tasks set model`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, values := args[0], args[1:]
			if key == "token_file" && len(values) == 1 {
				p, err := tokenPath(values[0])
				if err != nil {
					return err
				}
				values = []string{p}
			}
			req := &fleetv1.EditTasksRequest{Op: &fleetv1.EditTasksRequest_SetOption{SetOption: &fleetv1.TaskOption{Source: source, Key: key, Values: values}}}
			return editTasks(cmd, req, func([]*fleetv1.TaskSource) string {
				if len(values) == 0 {
					return "unset " + key
				}
				if key == "token_file" || key == "token_env" {
					return "set " + key
				}
				return fmt.Sprintf("set %s to %s", key, strings.Join(values, " "))
			})
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "the [[tasks]] source, when there are several")
	return cmd
}
