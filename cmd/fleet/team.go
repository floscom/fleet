package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/gateway"
	"fleet/internal/web"
)

func newTeamCmd() *cobra.Command {
	var teamRef string
	cmd := &cobra.Command{
		Use:     "team",
		Aliases: []string{"teams"},
		Short:   "Share skills with other people's fleets through a team hub",
		Long: `A team joins several people's fleets for the skills each member publishes,
through a hub anyone can run (fleet hub). Everything goes end-to-end
encrypted: the hub cannot read skill names, contents or member names.

A team lives on the daemon of this machine, which talks to the hub and
installs the team's skills into ~/.fleet/skills; the fleet sync takes them
to the fleet's other machines. Nothing is shared until it is published:
fleet team publish <skill>. What others publish is installed once you
subscribe to it (fleet team subscribe <member>/<skill> or <member>/*), and
updates come by themselves unless pinned. Changing a team skill here makes
it your own: it no longer updates. Deleting one unsubscribes it.

--team takes a team name or id prefix; it can be left out when this fleet
is in one team only.`,
		Example: `  fleet team create https://hub.example.com codeline --code ABCD-EFGH-JKMN
  fleet team invite                      # send the invite to a teammate
  fleet team join 'fleet-team:https://hub.example.com/...'
  fleet team publish git-commit
  fleet team skills
  fleet team subscribe flo/git-commit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listTeams(cmd) },
	}
	cmd.PersistentFlags().StringVar(&teamRef, "team", "", "the team: its name or id prefix")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List this fleet's teams",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listTeams(cmd) },
	}

	var code, as string
	create := &cobra.Command{
		Use:   "create <hub-url> <name>",
		Short: "Create a team on a hub, with this fleet as its admin",
		Long: `Create a team on a hub. This fleet becomes its first member and admin.
A hub that is not open needs a one-time creation code from its operator
(fleet hub code on the hub's machine).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Team gateway.TeamView `json:"team"`
			}
			body := map[string]string{"hub": args[0], "name": args[1], "code": code, "me": memberName(as)}
			if err := teamCall(cmd, http.MethodPost, "/api/teams", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created team %s (%s) as %s\ninvite teammates with: fleet team invite\n", out.Team.Name, shortID(out.Team.ID), out.Team.Me.Name)
			return nil
		},
	}
	create.Flags().StringVar(&code, "code", "", "the hub operator's team creation code")
	create.Flags().StringVar(&as, "as", "", "your name in the team (default: your user name)")

	var ttl string
	invite := &cobra.Command{
		Use:   "invite",
		Short: "Make an invite to the team (admins)",
		Long: `Print an invite for one person to join the team. Send it to them directly
(chat, mail), not through the hub. It is good for one join, for --ttl (7
days by default, at most 30). It is the approval as well: when they join,
this fleet's daemon adds them the next time it talks to the hub.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			var out struct {
				Invite    string `json:"invite"`
				ExpiresMs int64  `json:"expiresMs"`
			}
			if err := teamCall(cmd, http.MethodPost, teamPath(t, "invites"), map[string]string{"ttl": ttl}, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out.Invite)
			fmt.Fprintf(cmd.ErrOrStderr(), "good for one person, until %s: they run fleet team join '<invite>'\n", time.UnixMilli(out.ExpiresMs).Local().Format("Mon 2 Jan 15:04"))
			return nil
		},
	}
	invite.Flags().StringVar(&ttl, "ttl", "", "how long the invite is good for, like 7d or 48h")

	var joinAs string
	join := &cobra.Command{
		Use:   "join <invite>",
		Short: "Join a team with an invite",
		Long: `Join a team with an invite from one of its admins. This fleet is a member
once the admin's daemon added it, which it does by itself as soon as it
is online; until then the team shows as waiting.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Team gateway.TeamView `json:"team"`
			}
			body := map[string]string{"invite": args[0], "me": memberName(joinAs)}
			if err := teamCall(cmd, http.MethodPost, "/api/teams/join", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "asked to join as %s: waiting for an admin's fleet to add this one (fleet team shows when)\n", body["me"])
			return nil
		},
	}
	join.Flags().StringVar(&joinAs, "as", "", "your name in the team (default: your user name)")

	members := &cobra.Command{
		Use:   "members",
		Short: "List the team's members",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			tb := newTable(cmd.OutOrStdout(), "NAME", "ID", "ADMIN", "")
			for _, m := range t.Members {
				me := ""
				if m.Me {
					me = "(this fleet)"
				}
				tb.row(m.Name, shortID(m.ID), yesNo(m.Admin), me)
			}
			tb.flush()
			if len(t.Members) > 0 && countAdmins(t) == 1 {
				fmt.Fprintln(cmd.OutOrStdout(), "\nthe team has one admin: make a second one (fleet team admin <member>) so it survives losing that machine")
			}
			return nil
		},
	}

	var off bool
	admin := &cobra.Command{
		Use:   "admin <member>",
		Short: "Make a member an admin, or with --off no longer one (admins)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			if err := teamCall(cmd, http.MethodPost, teamPath(t, "members", args[0], "admin"), map[string]bool{"admin": !off}, nil); err != nil {
				return err
			}
			verb := "is an admin now"
			if off {
				verb = "is no longer an admin"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", args[0], verb)
			return nil
		},
	}
	admin.Flags().BoolVar(&off, "off", false, "take the admin role away")

	remove := &cobra.Command{
		Use:     "remove <member>",
		Aliases: []string{"rm"},
		Short:   "Remove a member from the team (admins)",
		Long: `Remove a member. The team gets a new key they never see, so they get
nothing published from now on; what they downloaded already they keep.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			if err := teamCall(cmd, http.MethodDelete, teamPath(t, "members", args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", args[0], t.Name)
			return nil
		},
	}

	leave := &cobra.Command{
		Use:   "leave",
		Short: "Leave the team",
		Long: `Leave the team. What you published is withdrawn; skills installed from
the team stay as they are and no longer update. The last admin cannot
leave: make someone else an admin first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			if err := teamCall(cmd, http.MethodPost, teamPath(t, "leave"), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "left %s\n", t.Name)
			return nil
		},
	}

	publish := &cobra.Command{
		Use:   "publish <skill>",
		Short: "Publish one of your skills to the team",
		Long: `Publish a skill of yours (from ~/.fleet/skills) to the team. Every later
change to it is sent too, until fleet team unpublish or deleting it.
Skills installed from a team cannot be published: only their publisher
does that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return teamOp(cmd, teamRef, "publish", map[string]string{"skill": args[0]}, "published %s to %s")
		},
	}
	unpublish := &cobra.Command{
		Use:   "unpublish <skill>",
		Short: "Withdraw a skill you published",
		Long:  `Withdraw a skill you published. Members who installed it lose it.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return teamOp(cmd, teamRef, "unpublish", map[string]string{"skill": args[0]}, "withdrew %s from %s")
		},
	}

	skillsCmd := &cobra.Command{
		Use:   "skills",
		Short: "List the skills members published",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(t.Skills) == 0 {
				fmt.Fprintln(out, "nobody published a skill yet")
				return nil
			}
			tb := newTable(out, "SKILL", "SIZE", "CHANGED", "STATE", "DESCRIPTION")
			for _, s := range t.Skills {
				tb.row(s.Ref, humanBytes(s.Size), age(s.ChangedMs), skillState(s), cut(s.Description, 60))
			}
			tb.flush()
			return nil
		},
	}

	subscribe := &cobra.Command{
		Use:   "subscribe <member>/<skill|*>",
		Short: "Install a member's skill, or all they publish, and keep it updated",
		Long: `Subscribe to a skill a member publishes (<member>/<skill>), or to all of
theirs (<member>/*): it is installed into ~/.fleet/skills and updated
whenever they change it. A skill's scripts run with your agents' rights,
so subscribe to people you trust. It also takes back skills it matches
that were unsubscribed or deleted here.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return teamOp(cmd, teamRef, "subscribe", map[string]string{"ref": args[0]}, "subscribed to %s in %s")
		},
	}
	unsubscribe := &cobra.Command{
		Use:   "unsubscribe <member>/<skill|*>",
		Short: "Stop a subscription and remove what it installed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return teamOp(cmd, teamRef, "unsubscribe", map[string]string{"ref": args[0]}, "unsubscribed from %s in %s")
		},
	}

	var unpin bool
	pin := &cobra.Command{
		Use:   "pin <member>/<skill>",
		Short: "Keep the installed version of a team skill; --off updates it again",
		Long: `Pin a team skill: updates wait until you look at them (fleet team diff)
and apply them (fleet team apply). --off unpins it and installs the latest
version.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := "pinned %s in %s"
			if unpin {
				msg = "unpinned %s in %s"
			}
			return teamOp(cmd, teamRef, "pin", map[string]any{"ref": args[0], "pin": !unpin}, msg)
		},
	}
	pin.Flags().BoolVar(&unpin, "off", false, "unpin, and install the latest version")

	diff := &cobra.Command{
		Use:   "diff <member>/<skill>",
		Short: "Show what the update waiting for a pinned skill changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			var out struct {
				Diff gateway.DiffView `json:"diff"`
			}
			if err := teamCall(cmd, http.MethodGet, teamPath(t, "diff")+"/"+escapeRef(args[0]), nil, &out); err != nil {
				return err
			}
			d := out.Diff
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: installed version from %s ago, update from %s ago\n", d.Skill, age(d.FromMs), age(d.ToMs))
			fmt.Fprint(cmd.OutOrStdout(), d.Text)
			if d.Truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "(the diff is cut short)")
			}
			return nil
		},
	}
	apply := &cobra.Command{
		Use:   "apply <member>/<skill>",
		Short: "Install the update waiting for a pinned skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return teamOp(cmd, teamRef, "apply", map[string]string{"bundle": args[0]}, "updated %s in %s")
		},
	}

	sync := &cobra.Command{
		Use:   "sync",
		Short: "Talk to the team's hub now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := pickTeam(cmd, teamRef)
			if err != nil {
				return err
			}
			if err := teamCall(cmd, http.MethodPost, teamPath(t, "sync"), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synced %s\n", t.Name)
			return nil
		},
	}

	cmd.AddCommand(ls, create, invite, join, members, admin, remove, leave, publish, unpublish, skillsCmd, subscribe, unsubscribe, pin, diff, apply, sync)
	return cmd
}

func listTeams(cmd *cobra.Command) error {
	var out struct {
		Teams []gateway.TeamView `json:"teams"`
	}
	if err := teamCall(cmd, http.MethodGet, "/api/teams", nil, &out); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if len(out.Teams) == 0 {
		fmt.Fprintln(w, "this fleet is in no team: fleet team create <hub-url> <name>, or fleet team join <invite>")
		return nil
	}
	tb := newTable(w, "TEAM", "ID", "STATUS", "AS", "MEMBERS", "PUBLISHED", "INSTALLED", "HUB")
	for _, t := range out.Teams {
		installed := 0
		for _, s := range t.Skills {
			if s.Installed != "" {
				installed++
			}
		}
		as := t.Me.Name
		if t.Admin {
			as += " (admin)"
		}
		status := t.Status
		if t.Error != "" && t.Status != gateway.StatusWaiting {
			status += ": " + cut(t.Error, 50)
		}
		tb.row(t.Name, shortID(t.ID), status, as, fmt.Sprint(len(t.Members)), fmt.Sprint(len(t.Published)), fmt.Sprint(installed), t.Hub)
	}
	tb.flush()
	return nil
}

// teamOp runs a POST /api/teams/{t}/<op> and prints msg with the first
// body value and the team's name.
func teamOp(cmd *cobra.Command, ref, op string, body any, msg string) error {
	t, err := pickTeam(cmd, ref)
	if err != nil {
		return err
	}
	if err := teamCall(cmd, http.MethodPost, teamPath(t, op), body, nil); err != nil {
		return err
	}
	var arg any
	switch b := body.(type) {
	case map[string]string:
		arg = firstValue(b)
	case map[string]any:
		arg = b["ref"]
	}
	fmt.Fprintf(cmd.OutOrStdout(), msg+"\n", arg, t.Name)
	return nil
}

func firstValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}

// skillState sums up a team skill for fleet team skills.
func skillState(s gateway.SkillView) string {
	var parts []string
	switch {
	case s.Mine:
		parts = append(parts, "yours")
	case s.Forked != "":
		parts = append(parts, "forked as "+s.Forked)
	case s.Installed != "" && s.Installed != s.Name:
		parts = append(parts, "installed as "+s.Installed)
	case s.Installed != "":
		parts = append(parts, "installed")
	case s.Dropped:
		parts = append(parts, "unsubscribed")
	case s.Subscribed:
		parts = append(parts, "subscribed")
	default:
		parts = append(parts, "-")
	}
	if s.Pinned {
		parts = append(parts, "pinned")
	}
	if s.Pending != nil {
		parts = append(parts, "update waiting")
	}
	if s.PublisherLeft {
		parts = append(parts, "publisher left")
	}
	if s.Problem != "" {
		parts = append(parts, s.Problem)
	}
	return strings.Join(parts, ", ")
}

func countAdmins(t gateway.TeamView) int {
	n := 0
	for _, m := range t.Members {
		if m.Admin {
			n++
		}
	}
	return n
}

// memberName is the name to join or create a team as: --as, else the
// user's name (the person, not the machine: config's name is the
// machine's), else config's name.
func memberName(as string) string {
	if as != "" {
		return as
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if cfg, err := config.Load(); err == nil {
		return cfg.Name
	}
	return "me"
}

// pickTeam finds the team --team names, or the only one.
func pickTeam(cmd *cobra.Command, ref string) (gateway.TeamView, error) {
	var out struct {
		Teams []gateway.TeamView `json:"teams"`
	}
	if err := teamCall(cmd, http.MethodGet, "/api/teams", nil, &out); err != nil {
		return gateway.TeamView{}, err
	}
	if ref == "" {
		switch len(out.Teams) {
		case 0:
			return gateway.TeamView{}, errors.New("this fleet is in no team: fleet team create <hub-url> <name>, or fleet team join <invite>")
		case 1:
			return out.Teams[0], nil
		}
		var names []string
		for _, t := range out.Teams {
			names = append(names, t.Name)
		}
		return gateway.TeamView{}, fmt.Errorf("this fleet is in %d teams (%s): say which with --team", len(out.Teams), strings.Join(names, ", "))
	}
	var hits []gateway.TeamView
	for _, t := range out.Teams {
		if t.ID == ref || strings.EqualFold(t.Name, ref) {
			return t, nil
		}
		if strings.HasPrefix(t.ID, strings.ToLower(ref)) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return gateway.TeamView{}, fmt.Errorf("no team %q", ref)
	case 1:
		return hits[0], nil
	}
	return gateway.TeamView{}, fmt.Errorf("%q names %d teams: use more of the id", ref, len(hits))
}

func teamPath(t gateway.TeamView, parts ...string) string {
	p := "/api/teams/" + t.ID
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// escapeRef escapes a <member>/<skill> ref for the diff route, which
// takes the rest of the path.
func escapeRef(ref string) string {
	owner, name, ok := strings.Cut(ref, "/")
	if !ok {
		return url.PathEscape(ref)
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// teamCall calls the local daemon's team API (web/teams.go) on loopback
// with the admin token: a team lives on this machine's daemon.
func teamCall(cmd *cobra.Command, method, path string, body, out any) error {
	if !client.IsLocal(host) {
		return errors.New("fleet team works with this machine's daemon, where its teams live: run it without --host")
	}
	base, err := localWebBase()
	if err != nil {
		return err
	}
	// The daemon reads the token file on every request, so one made here
	// works at once, as with fleet web.
	if _, err := config.EnsureHome(); err != nil {
		return err
	}
	tok, err := web.LoadOrCreateToken(config.Path("web-token"))
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("the fleet daemon is not running, or its dashboard is off (fleet start)")
		}
		return err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(reply, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("the daemon answered %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(reply, out)
	}
	return nil
}

// localWebBase is the URL of this machine's dashboard, from config.toml
// as the daemon reads it, on loopback when it listens on every address.
func localWebBase() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	addr := cfg.Web
	if addr == "" {
		addr = config.DefaultWeb
	}
	if addr == "off" {
		return "", errors.New(`teams are managed through the daemon's dashboard, which is off (web = "off" in config.toml)`)
	}
	h, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("web address %q in config.toml: %w", addr, err)
	}
	if ip := net.ParseIP(h); h == "" || ip != nil && ip.IsUnspecified() {
		h = "127.0.0.1"
		if ip != nil && ip.To4() == nil {
			h = "::1"
		}
	}
	return "http://" + net.JoinHostPort(h, port), nil
}

// humanBytes renders a size compactly: 812 B, 4.2 KiB, 1.3 MiB.
func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// cut shortens s to n runes with an ellipsis.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
