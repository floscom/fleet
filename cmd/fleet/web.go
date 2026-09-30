package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/web"
)

func newWebCmd() *cobra.Command {
	var rotate bool
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Print the admin link of this machine's web dashboard",
		Long: `Print links that open this machine's web dashboard with admin rights:
browsing its folders and adding or removing roots. Without the token the
dashboard is a read-only view.

The link carries this machine's admin token. Open it once in each browser
you manage the fleet from; the browser keeps it. Anyone holding it can
change this fleet's roots and list its folders, and the dashboard is plain
HTTP, so use it on networks you trust. --rotate replaces the token and
signs every browser out.

It also prints the command that joins other machines to this fleet
(fleet start --join KEY): this dashboard can then add roots on them, and
theirs on this one. The key stays in ~/.fleet/fleet-key; delete that file
and run fleet web again for a new one (then join the other machines again).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !client.IsLocal(host) {
				return fmt.Errorf("fleet web prints this machine's admin token; run it on %s instead", host)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			addr := cfg.Web
			if addr == "" {
				addr = config.DefaultWeb
			}
			if addr == "off" {
				return errors.New(`the web dashboard is off (web = "off" in config.toml)`)
			}
			bind, port, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("web address %q in config.toml: %w", addr, err)
			}
			if _, err := config.EnsureHome(); err != nil {
				return err
			}
			token := web.LoadOrCreateToken
			if rotate {
				token = web.RotateToken
			}
			tok, err := token(config.Path("web-token"))
			if err != nil {
				return err
			}
			key, err := web.LoadOrCreateToken(config.Path("fleet-key"))
			if err != nil {
				return err
			}
			printWebLinks(cmd.OutOrStdout(), bind, port, tok, rotate)
			printJoin(cmd.OutOrStdout(), key)
			return nil
		},
	}
	cmd.Flags().BoolVar(&rotate, "rotate", false, "replace the admin token, signing every browser out")
	return cmd
}

func printWebLinks(out io.Writer, bind, port, tok string, rotated bool) {
	hostname, _ := os.Hostname()
	if rotated {
		fmt.Fprintln(out, "New admin token: every browser is signed out.")
	}
	fmt.Fprintf(out, "Admin links for the fleet dashboard on %s (open one once per browser):\n\n", orDash(hostname))
	hosts, local := webHosts(bind)
	for _, h := range hosts {
		fmt.Fprintf(out, "    http://%s/#token=%s\n", net.JoinHostPort(h, port), tok)
	}
	if local {
		fmt.Fprintf(out, "\nThe dashboard listens on %s only. From another machine, forward it first:\n\n", bind)
		fmt.Fprintf(out, "    ssh -L %s:%s %s\n", port, net.JoinHostPort(bind, port), orDash(hostname))
	}
	fmt.Fprintln(out, "\nAnyone with the link can change this fleet's roots and list its folders.")
	if !rotated {
		fmt.Fprintln(out, "fleet web --rotate signs every browser out.")
	}
}

func printJoin(out io.Writer, key string) {
	fmt.Fprintln(out, "\nTo manage other machines from this dashboard, start fleet on them with:")
	fmt.Fprintf(out, "\n    %s\n", web.JoinCommand(key))
}

// webHosts lists the hosts a browser can use to reach a dashboard bound to
// bind, and whether it is reachable from this machine only.
func webHosts(bind string) (hosts []string, local bool) {
	if bind == "localhost" {
		return []string{"localhost"}, true
	}
	ip := net.ParseIP(bind)
	switch {
	case bind == "":
		// ":7421" listens on every address.
	case ip == nil:
		return []string{bind}, false // a host name
	case ip.IsLoopback():
		return []string{"localhost"}, true
	case !ip.IsUnspecified():
		return []string{bind}, false
	}
	return append(lanIPv4s(), "localhost"), false
}

// lanIPv4s lists the IPv4 addresses of this machine's up, non-loopback
// interfaces, skipping container bridges.
func lanIPv4s() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isContainerIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}

func isContainerIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "lxc", "lxd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
