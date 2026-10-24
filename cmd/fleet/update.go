package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/update"
)

func newUpdateCmd() *cobra.Command {
	var check bool
	var auto string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update fleet to the newest release, or turn automatic updates on or off",
		Long: `update installs the newest fleet release over the daemon's binary; the
daemon restarts on it once no agent works (agents keep running). Without
a running daemon it updates this binary.

The daemon also does this by itself: it looks for a new release a few
minutes after it starts and every 6 hours. --auto off turns that off
(auto_update in config.toml), --auto on turns it back on. Development
builds never update.`,
		Example: `  fleet update              # install the newest release now
  fleet update --check      # only look
  fleet update --auto off   # no automatic updates
  fleet -H studio update    # update another machine's daemon`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := &fleetv1.UpdateRequest{Check: check, Install: !check && auto == ""}
			switch auto {
			case "":
			case "on", "true":
				req.SetAuto, req.Auto = true, true
			case "off", "false":
				req.SetAuto = true
			default:
				return fmt.Errorf("--auto: want on or off, not %q", auto)
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			c, err := dial(ctx)
			if errors.Is(err, client.ErrNotRunning) && client.IsLocal(host) {
				return updateHere(ctx, out, req)
			}
			if err != nil {
				return err
			}
			defer c.Close()
			st, err := c.Update(ctx, req)
			// Every field may be unset: a daemon that finds the request
			// invalid does not know it.
			if client.Code(err) == fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
				return errors.New("this daemon is too old to update itself: install the new release with install.sh and restart it")
			}
			if err != nil {
				return err
			}
			return reportUpdate(out, req, st, "the daemon restarts on it once no agent works")
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only look for a newer release")
	cmd.Flags().StringVar(&auto, "auto", "", "turn automatic updates `on` or off")
	return cmd
}

// updateHere does what req asks without a daemon: to config.toml and to
// this binary.
func updateHere(ctx context.Context, out io.Writer, req *fleetv1.UpdateRequest) error {
	if _, err := config.EnsureHome(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if req.SetAuto {
		cfg.AutoUpdate = &req.Auto
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	st := &fleetv1.UpdateStatus{Version: version, Auto: cfg.AutoUpdateOn()}
	switch {
	case !update.Released(version):
		st.Unsupported = "this is a development build (" + version + "); only releases update themselves"
	default:
		if err := update.Writable(exe); err != nil {
			st.Unsupported = err.Error()
		}
	}
	if req.Check || req.Install {
		src := update.Source{}
		tag, err := src.Latest(ctx)
		if err != nil {
			return err
		}
		st.Latest = tag
		if req.Install && st.Unsupported == "" && update.Newer(tag, version) {
			if err := src.Install(ctx, tag, exe); err != nil {
				return err
			}
			st.Installed = tag
		}
	}
	return reportUpdate(out, req, st, "start the daemon with `fleet start`")
}

// reportUpdate prints what came of req; then says what happens to an
// installed update.
func reportUpdate(out io.Writer, req *fleetv1.UpdateRequest, st *fleetv1.UpdateStatus, then string) error {
	if req.SetAuto {
		fmt.Fprintf(out, "automatic updates: %s\n", onOff(st.Auto))
	}
	if !req.Check && !req.Install {
		if st.Unsupported != "" && st.Auto {
			fmt.Fprintf(out, "note: %s\n", st.Unsupported)
		}
		return nil
	}
	if st.Error != "" {
		return errors.New(st.Error)
	}
	switch {
	case st.Installed != "":
		fmt.Fprintf(out, "fleet %s is installed (running %s); %s\n", st.Installed, st.Version, then)
	case update.Newer(st.Latest, st.Version):
		fmt.Fprintf(out, "fleet %s is available (running %s)\n", st.Latest, st.Version)
		if req.Install && st.Unsupported != "" {
			return errors.New(st.Unsupported)
		}
		if !req.Install {
			fmt.Fprintln(out, "`fleet update` installs it")
		}
	case st.Unsupported != "" && !update.Released(st.Version):
		if req.Install {
			return fmt.Errorf("%s (the newest release is %s)", st.Unsupported, st.Latest)
		}
		fmt.Fprintf(out, "the newest release is %s; %s\n", st.Latest, st.Unsupported)
	default:
		fmt.Fprintf(out, "fleet %s is up to date\n", st.Version)
	}
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// updateSummary renders the status line of `fleet status`.
func updateSummary(st *fleetv1.UpdateStatus) string {
	switch {
	case st.Installed != "":
		return st.Installed + " installed, restarts once no agent works"
	case update.Newer(st.Latest, st.Version):
		switch {
		case st.Error != "":
			return st.Latest + " available; the update failed: " + st.Error
		case st.Unsupported != "":
			return st.Latest + " available; " + st.Unsupported
		}
		return st.Latest + " available; `fleet update` installs it"
	case !st.Auto:
		return "automatic updates off"
	case st.Unsupported != "":
		return "automatic, but not here: " + st.Unsupported
	case st.Error != "":
		return "automatic; last look failed: " + st.Error
	}
	return "automatic"
}
