package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/adapter/claude"
	"fleet/internal/adapter/codex"
	"fleet/internal/adapter/shell"
	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/daemon"
)

const (
	startTimeout = 10 * time.Second
	stopTimeout  = 15 * time.Second
)

// daemonFlags are shared by `fleet daemon` and `fleet start`.
type daemonFlags struct {
	listen string
	web    string
	noMDNS bool
}

func (f *daemonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.listen, "listen", "", `TCP listen address for paired devices, or "off" (default from config.toml, else `+config.DefaultListen+`)`)
	cmd.Flags().StringVar(&f.web, "web", "", `address of the web dashboard, or "off" (default from config.toml, else `+config.DefaultWeb+`)`)
	cmd.Flags().BoolVar(&f.noMDNS, "no-mdns", false, "do not advertise on the LAN")
}

func (f *daemonFlags) args() []string {
	var a []string
	if f.listen != "" {
		a = append(a, "--listen", f.listen)
	}
	if f.web != "" {
		a = append(a, "--web", f.web)
	}
	if f.noMDNS {
		a = append(a, "--no-mdns")
	}
	return a
}

// registry builds the adapter registry, applying [adapter.<id>] binary
// overrides. [adapter.<id>].args are not given to the adapters: the daemon
// prepends them to every launch's extra args itself (for every adapter, and
// picking up config reloads), so passing them here would duplicate them.
func registry(cfg *config.Config) *adapter.Registry {
	ac := func(id string) config.AdapterConfig { return cfg.Adapters[id] }
	return adapter.NewRegistry(
		claude.New(ac(claude.ID).Binary, nil),
		codex.New(ac(codex.ID).Binary, nil),
		shell.New(ac(shell.ID).Binary),
	)
}

// executable is the resolved path of the running fleet binary.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func newDaemonCmd() *cobra.Command {
	var f daemonFlags
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the fleet daemon in the foreground (logs to stderr)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if os.Getenv("FLEET_DEBUG") != "" {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
			if _, err := config.EnsureHome(); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			exe, err := executable()
			if err != nil {
				return fmt.Errorf("locate fleet binary: %w", err)
			}
			return daemon.Run(cmd.Context(), daemon.Options{
				Adapters:    registry(cfg),
				Version:     version,
				FleetBinary: exe,
				Listen:      f.listen,
				Web:         f.web,
				NoMDNS:      f.noMDNS,
			})
		},
	}
	f.register(cmd)
	return cmd
}

// requireLocal rejects --host for commands that manage this machine only.
func requireLocal(name string) error {
	if !client.IsLocal(host) {
		return fmt.Errorf("%s only manages the daemon on this machine; drop --host", name)
	}
	return nil
}

// localAlive reports whether the local daemon answers.
func localAlive(ctx context.Context) (*fleetv1.GetInfoResponse, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c, err := client.DialLocal(ctx)
	if err != nil {
		return nil, false
	}
	defer c.Close()
	info, err := c.GetInfo(ctx)
	return info, err == nil
}

func newStartCmd() *cobra.Command {
	var f daemonFlags
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the fleet daemon in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocal("start"); err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if _, ok := localAlive(ctx); ok {
				fmt.Fprintf(out, "fleet daemon is already running%s\n", pidSuffix())
				return nil
			}
			if _, err := config.EnsureHome(); err != nil {
				return err
			}
			exe, err := executable()
			if err != nil {
				return err
			}
			logPath := config.Path("daemon.log")
			logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			defer logf.Close()
			child := exec.Command(exe, append([]string{"daemon"}, f.args()...)...)
			child.Stdout, child.Stderr = logf, logf
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := child.Start(); err != nil {
				return fmt.Errorf("start daemon: %w", err)
			}
			exited := make(chan error, 1)
			go func() { exited <- child.Wait() }()

			deadline := time.NewTimer(startTimeout)
			defer deadline.Stop()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case err := <-exited:
					return fmt.Errorf("daemon exited during startup (%v); last log lines:\n%s", err, tail(logPath, 10))
				case <-deadline.C:
					_ = child.Process.Signal(syscall.SIGTERM)
					return fmt.Errorf("daemon did not answer within %s; see %s", startTimeout, logPath)
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
				}
				if _, ok := localAlive(ctx); !ok {
					continue
				}
				pidPath := config.Path("daemon.pid")
				if _, err := os.Stat(pidPath); errors.Is(err, os.ErrNotExist) {
					_ = os.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600)
				}
				fmt.Fprintf(out, "fleet daemon started (pid %d), log: %s\n", child.Process.Pid, logPath)
				return nil
			}
		},
	}
	f.register(cmd)
	return cmd
}

// readPid returns the pid recorded in daemon.pid (0 if none).
func readPid() int {
	b, err := os.ReadFile(config.Path("daemon.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func pidSuffix() string {
	if pid := readPid(); pid > 0 {
		return fmt.Sprintf(" (pid %d)", pid)
	}
	return ""
}

// processAlive reports whether pid exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func newStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background fleet daemon (agents keep running in tmux)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocal("stop"); err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			pid := readPid()
			_, alive := localAlive(ctx)
			switch {
			case pid == 0 && alive:
				return errors.New("daemon is running but daemon.pid is missing; stop it where it was started")
			case pid == 0 || !processAlive(pid):
				// A stale daemon.pid is left alone: the next daemon takes
				// its lock, and unlinking a file another daemon may be
				// locking right now would let two daemons run.
				fmt.Fprintln(out, "fleet daemon is not running")
				return nil
			case !alive && !force:
				// A recycled pid must not get a signal meant for the daemon.
				return fmt.Errorf("pid %d from daemon.pid is alive but no daemon answers on %s; use --force to signal it anyway", pid, config.SocketPath())
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			deadline := time.Now().Add(stopTimeout)
			for processAlive(pid) {
				if time.Now().After(deadline) {
					return fmt.Errorf("daemon (pid %d) did not exit within %s", pid, stopTimeout)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
			fmt.Fprintf(out, "fleet daemon stopped (pid %d)\n", pid)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "signal the recorded pid even if the daemon does not answer")
	return cmd
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon runs, and what it is doing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			c, err := dial(ctx)
			if errors.Is(err, client.ErrNotRunning) {
				fmt.Fprintln(out, "fleet daemon is not running")
				return exitCode(3)
			}
			if err != nil {
				return err
			}
			defer c.Close()
			info, err := c.GetInfo(ctx)
			if err != nil {
				return err
			}
			agents, err := c.ListAgents(ctx, false)
			if err != nil {
				return err
			}
			roots, err := c.ListRoots(ctx)
			if err != nil {
				return err
			}
			where := "local"
			if !client.IsLocal(host) {
				where = "remote"
			}
			fmt.Fprintf(out, "fleet daemon %s on %s (%s, %s/%s), %s", info.DaemonVersion, info.ServerName, info.Hostname, info.Os, info.Arch, where)
			if client.IsLocal(host) {
				fmt.Fprint(out, pidSuffix())
			}
			fmt.Fprintln(out)
			if info.StartedAtMs > 0 {
				fmt.Fprintf(out, "up:        %s\n", age(info.StartedAtMs))
			}
			fmt.Fprintf(out, "server id: %s\n", info.ServerId)
			fmt.Fprintf(out, "agents:    %s\n", agentSummary(agents))
			fmt.Fprintf(out, "roots:     %d\n", len(roots))
			if devs, err := c.ListDevices(ctx); err == nil {
				connected := 0
				for _, d := range devs {
					if d.Connected {
						connected++
					}
				}
				fmt.Fprintf(out, "devices:   %d paired, %d connected\n", len(devs), connected)
			}
			return nil
		},
	}
}

// agentSummary renders "3 (1 working, 2 idle)".
func agentSummary(agents []*fleetv1.Agent) string {
	if len(agents) == 0 {
		return "0"
	}
	counts := map[fleetv1.AgentState]int{}
	for _, a := range agents {
		counts[a.State]++
	}
	var parts []string
	for s := fleetv1.AgentState_AGENT_STATE_STARTING; s <= fleetv1.AgentState_AGENT_STATE_FAILED; s++ {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, stateName(s)))
		}
	}
	return fmt.Sprintf("%d (%s)", len(agents), strings.Join(parts, ", "))
}

// tail returns up to n last lines of the file at path.
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 64<<10 {
		_, _ = f.Seek(-64<<10, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
