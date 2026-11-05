package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"fleet/internal/adapter"
	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/skills"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "skill",
		Aliases: []string{"skills"},
		Short:   "Share skills with every machine in the fleet",
		Long: `Skills in ~/.fleet/skills are shared with every machine holding the
fleet key, and linked into each agent CLI's own skills folder there:
~/.claude/skills for Claude Code, ~/.agents/skills for Codex and Gemini CLI.

A skill is a folder with a SKILL.md and whatever it brings along (scripts,
references, assets). Drop one into ~/.fleet/skills, or use fleet skill add.
Changing or deleting a skill there changes or deletes it on every machine;
when two machines change one, the later change wins, as a whole.

Left out: folders not named like a skill (letters, digits, - and _), ones
without a SKILL.md, and inside a skill .git, node_modules, caches, links
and editor or OS litter. A skill is at most 16 MiB. A skill of your own of
the same name in a CLI's folder is never replaced: fleet skill ls shows it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listSkills(cmd) },
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the shared skills",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listSkills(cmd) },
	}

	var link bool
	add := &cobra.Command{
		Use:   "add <folder>",
		Short: "Share a skill with every machine in the fleet",
		Long: `Share a skill folder with every machine in the fleet, under its folder's
name.

A skill in an agent CLI's skills folder (~/.claude/skills, ~/.agents/skills)
is moved into ~/.fleet/skills, and linked back. Any other is copied, or with
--link stays where it is, say in a git repository of yours, and is linked
in: changes there are shared, and changes from other machines land there.`,
		Example: `  fleet skill add ~/.claude/skills/deploy
  fleet skill add --link ~/src/my-skills/review`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := localSkills(); err != nil {
				return err
			}
			name, how, err := skills.Add(config.Path("skills"), args[0], skillDirs(cmd), link)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "shared %s (%s into %s)\n", name, how, tilde(config.Path("skills")))
			keyHint(out)
			return nil
		},
	}
	add.Flags().BoolVar(&link, "link", false, "leave the folder where it is and link it in")

	rm := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Delete a shared skill, on every machine in the fleet",
		Long: `Delete a shared skill from ~/.fleet/skills and from every machine in the
fleet. Of a skill added with --link only the link goes; its folder stays.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := localSkills(); err != nil {
				return err
			}
			if err := skills.Remove(config.Path("skills"), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s, on every machine in the fleet\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(ls, add, rm)
	return cmd
}

func listSkills(cmd *cobra.Command) error {
	if err := localSkills(); err != nil {
		return err
	}
	dir := config.Path("skills")
	list, err := skills.Status(dir, config.Path("skills.json"), skillDirs(cmd))
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(list) == 0 {
		fmt.Fprintf(out, "no shared skills yet: put a skill folder into %s, or run fleet skill add <folder>\n", tilde(dir))
		keyHint(out)
		return nil
	}
	t := newTable(out, "NAME", "CHANGED", "ON", "LINKED INTO", "NOTE")
	for _, in := range list {
		changed, on := "-", "-"
		if in.Entry.Version != 0 {
			changed, on = age(in.Entry.Version), orDash(in.Entry.Host)
		}
		var linked []string
		for _, d := range in.Links {
			linked = append(linked, tilde(d))
		}
		var notes []string
		switch {
		case in.Problem != "":
			notes = append(notes, in.Problem)
		case in.Pending && in.Entry.Version == 0:
			notes = append(notes, "not shared yet (is the daemon running?)")
		case in.Pending:
			notes = append(notes, "changed, not shared yet (is the daemon running?)")
		}
		switch e := in.Entry; {
		case e.Source != nil:
			notes = append(notes, "from "+e.Source.Ref()+" in team "+cmp.Or(e.Source.TeamName, e.Source.Team))
		case e.ForkedFrom != nil:
			notes = append(notes, "forked from "+e.ForkedFrom.Ref())
		}
		if in.Target != "" {
			notes = append(notes, "linked from "+tilde(in.Target))
		}
		for _, d := range in.Conflicts {
			notes = append(notes, "another skill of that name in "+tilde(d))
		}
		t.row(in.Name, changed, on, orDash(strings.Join(linked, " ")), strings.Join(notes, "; "))
	}
	t.flush()
	keyHint(out)
	return nil
}

// localSkills refuses --host: skills are shared from this machine's folder.
func localSkills() error {
	if !client.IsLocal(host) {
		return errors.New("fleet skill works on this machine's folder (every machine in the fleet gets the same skills): run it without --host")
	}
	return nil
}

// skillDirs are the agent CLIs' skills folders on this machine.
func skillDirs(cmd *cobra.Command) []string {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	return adapter.SkillDirs(cmd.Context(), registry(cfg))
}

// keyHint says when skills stay on this machine for want of a fleet key.
func keyHint(out io.Writer) {
	if b, err := os.ReadFile(config.Path("fleet-key")); err != nil || strings.TrimSpace(string(b)) == "" {
		fmt.Fprintln(out, "this machine holds no fleet key, so its skills stay here: run fleet web to create one, then fleet start --join <key> on the others")
	}
}

// tilde shortens a path in the home directory to ~/...
func tilde(p string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(h, p); err == nil && filepath.IsLocal(rel) {
		return filepath.Join("~", rel)
	}
	return p
}
