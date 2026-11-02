package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"fleet/internal/hub"
)

func newHubCmd() *cobra.Command {
	var opts hub.ServeOptions
	var quotaMiB int64
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "Run a team hub: the server fleets share skills through",
		Long: `Run a team hub. A hub stores and forwards what members of a team publish
to each other: signed member lists, sealed team keys and sealed skill
bundles. It cannot read skill names, skill contents or member names.
Anyone can run one; a team picks its hub when it is created.

Without --autocert the hub serves plain HTTP on --listen, for a reverse
proxy that terminates TLS in front of it. With --autocert DOMAIN it serves
HTTPS itself on :443 with a Let's Encrypt certificate (and the ACME
challenge on :80).

Teams are created with a one-time code: run fleet hub code on the same
machine while the hub runs, and give the code to whoever creates the team
(fleet team create). --open lets anyone create teams.`,
		Example: `  fleet hub --autocert hub.example.com
  fleet hub --listen 127.0.0.1:7430      # behind nginx or caddy
  fleet hub code                         # a one-time team creation code`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Quota = quotaMiB << 20
			opts.Log = slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			return hub.Serve(cmd.Context(), opts)
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&opts.DataDir, "data", defaultHubDir(), "where the hub keeps its state")
	cmd.Flags().StringVar(&opts.Listen, "listen", hub.DefaultListen, "address to serve plain HTTP on (for a reverse proxy)")
	cmd.Flags().StringVar(&opts.Autocert, "autocert", "", "serve HTTPS on :443 with a Let's Encrypt certificate for this domain")
	cmd.Flags().BoolVar(&opts.Open, "open", false, "let anyone create a team, without a creation code")
	cmd.Flags().Int64Var(&quotaMiB, "quota", hub.DefaultQuota>>20, "per-team storage quota in MiB")
	cmd.Flags().BoolVar(&opts.TrustForwarded, "trust-proxy", false, "take client addresses from X-Forwarded-For (behind a reverse proxy)")

	code := &cobra.Command{
		Use:   "code",
		Short: "Print a one-time team creation code from the running hub",
		Long: `Ask the hub running on this machine for a one-time code that lets someone
create a team on it (fleet team create ... --code). The code is good for an
hour and for one team.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, err := hub.RequestCode(cmd.Context(), opts.DataDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", reply.Code)
			fmt.Fprintf(cmd.ErrOrStderr(), "good for one team, until %s\n", reply.Expires.Local().Format("15:04"))
			return nil
		},
	}
	cmd.AddCommand(code)
	return cmd
}

// defaultHubDir is ~/.fleet-hub: apart from the fleet home, as a hub is
// usually a machine of its own.
func defaultHubDir() string {
	if d := os.Getenv("FLEET_HUB_DATA"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".fleet-hub"
	}
	return filepath.Join(h, ".fleet-hub")
}
