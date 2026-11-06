package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"fleet/internal/client"
)

// copyTimeout bounds sending one file: the daemons stream it, so big files
// take long.
const copyTimeout = time.Hour

// sentFile is what POST /api/send answers (web/send.go).
type sentFile struct {
	Host string `json:"host"`
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Dir is the folder the file went to when one was asked for; "" from
	// a machine too old for --to, which kept it in its upload folder.
	Dir string `json:"dir"`
}

// peerInfo is a fleet daemon on the network, as GET /api/peers lists it
// (web.Peer).
type peerInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Addrs   []string `json:"addrs"`
	WebPort int      `json:"webPort"`
	Self    bool     `json:"self"`
}

func newCopyCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:     "cp <file>... <machine>",
		Aliases: []string{"copy"},
		Short:   "Send files to another machine of the fleet",
		Long: `cp sends files of this machine to another machine of the fleet, daemon to
daemon: name it as fleet machines lists it (its name, host name or the
start of its id). Both machines must hold the same fleet key (fleet start
--join KEY) and have their dashboard on the network.

There the files land in fleet's upload folder (kept 30 days), or with
--to in a folder of your choice, under their own names; a file of the
same name there is never replaced. cp prints where each one went, so an
agent can pass the path on, e.g. to an agent on that machine with
fleet --host <machine> send <agent> <text>.

Agents of the fleet can run it themselves to hand a file they made to
another machine. It talks to this machine's daemon, so --host does not
apply.`,
		Example: `  fleet cp report.pdf macbook
  fleet cp dist/app.tar.gz build/notes.md studio --to '~/Downloads'
  fleet cp out.png macbook --to /Users/flo/Desktop
  fleet machines`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := localOnly("fleet cp"); err != nil {
				return err
			}
			machine := args[len(args)-1]
			files := make([]string, 0, len(args)-1)
			for _, f := range args[:len(args)-1] {
				p, err := filepath.Abs(f)
				if err != nil {
					return err
				}
				fi, err := os.Stat(p)
				switch {
				case err != nil:
					return err
				case fi.IsDir():
					return fmt.Errorf("%s is a folder: send a .tar.gz or .zip of it", f)
				case !fi.Mode().IsRegular():
					return fmt.Errorf("%s is not a regular file", f)
				case fi.Size() == 0:
					return fmt.Errorf("%s is empty", f)
				}
				files = append(files, p)
			}
			// A "~" the shell left (quoted) is the other machine's home:
			// its daemon expands it.
			to := dir
			out := cmd.OutOrStdout()
			for _, p := range files {
				var r sentFile
				ctx, cancel := context.WithTimeout(cmd.Context(), copyTimeout)
				err := localWebCall(ctx, http.MethodPost, "/api/send", map[string]string{"path": p, "host": machine, "dir": to}, &r)
				cancel()
				if err != nil {
					return fmt.Errorf("%s: %w", filepath.Base(p), err)
				}
				fmt.Fprintf(out, "sent %s to %s: %s (%s)\n", filepath.Base(p), r.Name, r.Path, bytesIEC(uint64(r.Size)))
				if to != "" && r.Dir == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "fleet: %s is too old for --to: it kept the file in its upload folder\n", r.Name)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "to", "", `folder on that machine to put the files in: absolute, or '~/...' (quoted) in its home`)
	return cmd
}

func newMachinesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "machines",
		Short: "List the fleet's machines that fleet cp can send files to",
		Long: `machines lists the fleet daemons this machine's daemon sees on the LAN
and the tailnet, this one first. SEND tells whether fleet cp can reach a
machine: it needs its dashboard on the network.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := localOnly("fleet machines"); err != nil {
				return err
			}
			var r struct {
				Peers    []peerInfo `json:"peers"`
				Complete bool       `json:"complete"`
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := localWebCall(ctx, http.MethodGet, "/api/peers", nil, &r); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			t := newTable(out, "NAME", "ID", "HOST", "SEND")
			for _, p := range r.Peers {
				name, send := orDash(p.Name), yesNo(p.WebPort != 0 && len(p.Addrs) > 0)
				if p.Self {
					name += " (this machine)"
					send = "-"
				}
				t.row(name, orDash(shortID(p.ID)), orDash(p.Host), send)
			}
			t.flush()
			if !r.Complete {
				fmt.Fprintln(cmd.ErrOrStderr(), "fleet: still looking for machines on the network: run it again in a few seconds")
			}
			return nil
		},
	}
}

// localOnly fails when --host names another daemon: the command works
// with this machine's files and daemon. Agents run inside a Docker
// sandbox cannot reach it.
func localOnly(name string) error {
	if !client.IsLocal(host) {
		return errors.New(name + " works with this machine's daemon: run it without --host (or FLEET_HOST)")
	}
	if _, err := os.Stat("/.dockerenv"); err == nil && os.Getenv("FLEET_AGENT_ID") != "" {
		return errors.New(name + " does not work inside a Docker sandbox yet: run the agent without one")
	}
	return nil
}
