package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fleet/internal/config"
	"fleet/internal/service"
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run the daemon as a service that starts at boot (systemd, launchd)",
		Long: `service installs the daemon as a service of the OS, so it starts at boot
(macOS: at login) and comes back after a crash: a systemd user unit on
Linux, a launchd agent on macOS. Run as root, a system unit or launch
daemon instead.

Once it is installed, fleet start and fleet stop start and stop the
service. Stopping or restarting it never stops the agents: they keep
running in tmux, like after fleet stop.`,
		Example: `  fleet service install                 # install, enable and start it
  fleet service install --root ~/code   # and make ~/code a root
  fleet service                         # is it installed, does it run
  fleet service uninstall               # stop and remove it`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return serviceStatus(cmd.OutOrStdout()) },
	}
	cmd.AddCommand(newServiceInstallCmd(), newServiceUninstallCmd(), &cobra.Command{
		Use:   "status",
		Short: "Show whether the service is installed and runs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return serviceStatus(cmd.OutOrStdout()) },
	})
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	var f daemonFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the daemon as a service, enable and start it",
		Long: `install writes a service that runs "fleet daemon" with the given flags,
enables it and starts it. A daemon started with fleet start is stopped
first; its agents keep running and the service's daemon takes them over.
Running install again rewrites the service, e.g. with other flags.

The service gets this shell's PATH and locale, so the daemon finds tmux,
git and the agent CLIs as here. On Linux it turns on lingering for the
user (loginctl enable-linger), without which systemd stops user services,
and with them the agents, at logout.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocal("service install"); err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			roots, err := f.rootPaths()
			if err != nil {
				return err
			}
			if err := f.checkJoin(); err != nil {
				return err
			}
			m, err := service.New()
			if err != nil {
				return err
			}
			exe, err := executable()
			if err != nil {
				return err
			}
			if strings.Contains(exe, "go-build") {
				return fmt.Errorf("%s is a temporary build (go run): install fleet and run its install from there", exe)
			}
			if _, err := config.EnsureHome(); err != nil {
				return err
			}
			if changed, err := f.applyJoin(); err != nil {
				return err
			} else if changed {
				fmt.Fprintln(out, "joined the fleet: its dashboards can now manage this machine")
			}
			// A daemon the service does not run holds the pid lock, so
			// the service's own would exit at once.
			if st, err := m.Status(); err == nil && !st.Running {
				if _, alive := localAlive(ctx); alive {
					if err := stopLocal(ctx, out, false); err != nil {
						return fmt.Errorf("stop the running daemon first: %w", err)
					}
				}
			}
			logPath := config.Path("daemon.log")
			notes, err := m.Install(service.Spec{Binary: exe, Args: f.args(), Env: service.CurrentEnv(), Log: logPath})
			if err != nil {
				return err
			}
			if err := waitAlive(ctx, logPath); err != nil {
				return err
			}
			fmt.Fprintf(out, "installed the %s %s\n", m.Kind(), m.Path())
			fmt.Fprintf(out, "fleet daemon started%s, log: %s; it starts at %s\n", pidSuffix(), logPath, bootOrLogin(m))
			for _, n := range notes {
				fmt.Fprintln(out, "note:", n)
			}
			return addLocalRoots(ctx, out, roots, f.trust)
		},
	}
	f.register(cmd)
	return cmd
}

func newServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove it (agents keep running in tmux)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocal("service uninstall"); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			m, err := service.New()
			if err != nil {
				return err
			}
			if st, err := m.Status(); err == nil && !st.Installed {
				fmt.Fprintln(out, "no fleet service is installed")
				return nil
			}
			if err := m.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintf(out, "removed the %s %s; the daemon is stopped, agents keep running in tmux\n", m.Kind(), m.Path())
			fmt.Fprintln(out, "fleet start runs the daemon without a service")
			return nil
		},
	}
}

func serviceStatus(out io.Writer) error {
	if err := requireLocal("service"); err != nil {
		return err
	}
	m, err := service.New()
	if err != nil {
		return err
	}
	st, err := m.Status()
	if err != nil {
		return err
	}
	if !st.Installed {
		fmt.Fprintln(out, "no fleet service is installed; fleet service install sets one up")
		return nil
	}
	fmt.Fprintf(out, "%s %s\n", m.Kind(), m.Path())
	fmt.Fprintf(out, "state:   %s\n", serviceSummary(m, st))
	return nil
}

// serviceSummary renders "active (running), pid 123, starts at boot".
func serviceSummary(m service.Manager, st service.Status) string {
	s := st.Detail
	if s == "" {
		s = "stopped"
	}
	if st.Running && st.Pid > 0 {
		s += fmt.Sprintf(", pid %d", st.Pid)
	}
	if st.Enabled {
		return s + ", starts at " + bootOrLogin(m)
	}
	return s + ", disabled"
}

// installedService returns this machine's service manager and what it says
// about the fleet service; the status is zero without one.
func installedService() (service.Manager, service.Status) {
	m, err := service.New()
	if err != nil {
		return nil, service.Status{}
	}
	st, err := m.Status()
	if err != nil {
		// The unit file exists, but its manager does not answer.
		return m, service.Status{Installed: st.Installed}
	}
	return m, st
}

// bootOrLogin says when the service starts by itself.
func bootOrLogin(m service.Manager) string {
	if m.Kind() == "launchd agent" {
		return "login"
	}
	return "boot"
}

// waitAlive waits for the local daemon to answer, as after starting it.
func waitAlive(ctx context.Context, logPath string) error {
	deadline := time.Now().Add(startTimeout)
	for {
		if _, ok := localAlive(ctx); ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not answer within %s; last log lines:\n%s", startTimeout, tail(logPath, 10))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
