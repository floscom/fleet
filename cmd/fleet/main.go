// Command fleet is both the fleet daemon ("fleet daemon") and its CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"fleet/internal/client"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// host is the global --host/-H target ("" = local daemon).
var host string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		var ex exitCode
		if errors.As(err, &ex) {
			os.Exit(int(ex))
		}
		fmt.Fprintln(os.Stderr, "fleet:", err)
		os.Exit(1)
	}
}

// exitCode lets a command choose the process exit status; nothing is printed.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "fleet",
		Short: "Run and watch coding agents (Claude Code, Codex, ...) in tmux, locally or on a paired server",
		Long: `fleet runs agent CLIs in tmux sessions managed by a daemon, and lets
paired devices on the LAN start, watch and attach to them.

Every command that talks to a daemon uses the local one unless --host
(or FLEET_HOST) names a paired server.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&host, "host", "H", os.Getenv("FLEET_HOST"),
		`daemon to talk to: paired server name, id prefix or address ("" or "local" = this machine)`)
	root.AddCommand(
		newDaemonCmd(), newStartCmd(), newStopCmd(), newStatusCmd(), newVersionCmd(),
		newPairCmd(), newConnectCmd(), newDiscoverCmd(), newServersCmd(), newDevicesCmd(),
		newRootsCmd(), newBrowseCmd(), newAdaptersCmd(),
		newLsCmd(), newRunCmd(), newAttachCmd(), newSendCmd(), newKillCmd(), newWatchCmd(),
		newSandboxCmd(), newWebCmd(), newHookCmd(),
	)
	return root
}

// dial connects to the daemon selected by --host.
func dial(ctx context.Context) (*client.Client, error) {
	return client.Connect(ctx, host)
}

// withClient runs fn with a connection to the selected daemon.
func withClient(cmd *cobra.Command, fn func(ctx context.Context, c *client.Client) error) error {
	ctx := cmd.Context()
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(ctx, c)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the fleet version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "fleet %s (protocol v1)\n", version)
		},
	}
}
