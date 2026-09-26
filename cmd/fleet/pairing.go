package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"fleet/internal/client"
	"fleet/internal/config"
	"fleet/internal/discovery"
	"fleet/internal/identity"
)

const (
	connectTimeout = 30 * time.Second
	nameLookupTime = 1500 * time.Millisecond
)

func newPairCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Create a one-time code to pair a new device with this daemon",
		Long: `Run this on the machine running the daemon. On the new device run
"fleet connect <this host> --code <code>" (or enter the code in the app).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				r, err := c.CreatePairingCode(ctx, ttl)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				expires := time.UnixMilli(r.ExpiresAtMs)
				fmt.Fprintf(out, "Pairing code for %s (single use, expires %s, in %s):\n\n", c.ServerName(),
					expires.Format("15:04:05"), compactDuration(time.Until(expires).Round(time.Second)))
				fmt.Fprintf(out, "        %s\n\n", spaced(r.Code))
				fmt.Fprintf(out, "Server fingerprint: %s\n", fingerprint(r.ServerId))
				fmt.Fprintln(out, "(the new device must confirm this fingerprint before it sends the code)")
				fmt.Fprintln(out)
				target, warn := pairHint()
				if warn != "" {
					fmt.Fprintln(out, "Warning:", warn)
				}
				fmt.Fprintf(out, "On the other machine run:\n\n    fleet connect %s --code %s --fingerprint %s\n",
					target, r.Code, r.ServerId[:min(len(r.ServerId), client.MinFingerprintLen)])
				return nil
			})
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 5*time.Minute, "how long the code stays valid (at most 5m)")
	return cmd
}

// spaced renders "ABCD-EFGH" as "A B C D - E F G H" for legibility.
func spaced(code string) string {
	return strings.Join(strings.Split(code, ""), " ")
}

// pairHint returns the address other machines should use, plus a warning
// when the local daemon does not accept remote connections.
func pairHint() (target, warn string) {
	if !client.IsLocal(host) {
		return host, ""
	}
	target, _ = os.Hostname()
	if target == "" {
		target = "<this-host>"
	}
	cfg, err := config.Load()
	if err != nil {
		return target, ""
	}
	if cfg.Listen == "" || cfg.Listen == "off" {
		return target, `remote connections are disabled (listen in config.toml); start the daemon with --listen`
	}
	if _, port, err := net.SplitHostPort(cfg.Listen); err == nil && port != strconv.Itoa(client.DefaultPort) {
		target = net.JoinHostPort(target, port)
	}
	return target, ""
}

func newConnectCmd() *cobra.Command {
	var code, name, fp string
	cmd := &cobra.Command{
		Use:   "connect [addr|name] --code XXXX-XXXX [--fingerprint HEX]",
		Short: "Pair this machine with a remote fleet daemon",
		Long: `Pairs with a daemon using a code from "fleet pair" on that machine.
The target is host:port, a host (port 7420), the daemon's name as seen by
"fleet discover", or omitted to use the only daemon found on the LAN.

The server fingerprint shown by "fleet pair" is checked before the code is
sent: pass it with --fingerprint, or confirm it when asked.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if identity.NormalizeCode(code) == "" {
				return client.ErrInvalidCode
			}
			verify, err := fingerprintCheck(cmd, fp)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), connectTimeout)
			defer cancel()
			var target string
			if len(args) == 1 {
				target = args[0]
			}
			addrs, err := resolveConnectTarget(ctx, target)
			if err != nil {
				return err
			}
			if name == "" {
				name, _ = os.Hostname()
			}
			dev, err := client.LoadDevice()
			if err != nil {
				return err
			}
			known, err := pairAny(ctx, addrs, code, name, dev, verify)
			if err != nil {
				return err
			}
			store, err := client.OpenServers()
			if err != nil {
				return err
			}
			if err := store.Put(known); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Paired with %s at %s\n", known.Name, known.Address)
			fmt.Fprintf(out, "Server fingerprint: %s\n\n", fingerprint(known.ID))
			fmt.Fprintf(out, "Try: fleet -H %s ls\n", known.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "pairing code shown by `fleet pair` (required)")
	cmd.Flags().StringVar(&name, "name", "", "name for this device (default: hostname)")
	cmd.Flags().StringVar(&fp, "fingerprint", "", fmt.Sprintf("server fingerprint shown by `fleet pair` (at least %d hex digits)", client.MinFingerprintLen))
	_ = cmd.MarkFlagRequired("code")
	return cmd
}

// resolveConnectTarget turns a connect argument into addresses to try.
func resolveConnectTarget(ctx context.Context, target string) ([]string, error) {
	if strings.Contains(target, ":") {
		return []string{client.HostPort(target)}, nil
	}
	wait := nameLookupTime
	if target == "" {
		wait = 2 * time.Second
	}
	found, err := discovery.Browse(ctx, wait)
	if err != nil && target == "" {
		return nil, fmt.Errorf("LAN discovery failed (%v); pass an address", err)
	}
	if target == "" {
		switch len(found) {
		case 0:
			return nil, errors.New("no fleet daemons found on the LAN; pass an address (host or host:port)")
		case 1:
			return foundAddrs(found[0]), nil
		default:
			names := make([]string, len(found))
			for i, f := range found {
				names[i] = f.Name
			}
			return nil, fmt.Errorf("several daemons found (%s); pass a name or address", strings.Join(names, ", "))
		}
	}
	for _, f := range found {
		if f.Name == target || f.Instance == target || (len(target) >= 6 && strings.HasPrefix(f.ServerID, strings.ToLower(target))) {
			return foundAddrs(f), nil
		}
	}
	return []string{client.HostPort(target)}, nil
}

func foundAddrs(f discovery.Found) []string {
	out := make([]string, len(f.Addrs))
	for i, ip := range f.Addrs {
		out[i] = net.JoinHostPort(ip, strconv.Itoa(f.Port))
	}
	return out
}

// fingerprintCheck returns the server check for `fleet connect`: the
// --fingerprint value if given, else an interactive confirmation.
func fingerprintCheck(cmd *cobra.Command, want string) (func(string) error, error) {
	if want != "" {
		return client.MatchFingerprint(want)
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return nil, errors.New("pass --fingerprint with the server fingerprint shown by `fleet pair` (it is checked before the code is sent)")
	}
	return func(serverID string) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "Server fingerprint: %s\nDoes it match the fingerprint shown by `fleet pair`? [y/N] ", fingerprint(serverID))
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return client.ErrFingerprintRejected
		}
		return nil
	}, nil
}

// pairAny tries each address until one is reachable. Rejections by the
// daemon, failed server proofs and rejected fingerprints are final.
func pairAny(ctx context.Context, addrs []string, code, name string, dev *identity.Device, verify func(string) error) (identity.KnownServer, error) {
	var lastErr error = errors.New("no address to try")
	for _, addr := range addrs {
		known, err := client.PairVerify(ctx, addr, code, name, dev, verify)
		if err == nil {
			return known, nil
		}
		var e *client.Error
		if errors.As(err, &e) || errors.Is(err, client.ErrServerProof) || errors.Is(err, client.ErrFingerprintRejected) || ctx.Err() != nil {
			return identity.KnownServer{}, err
		}
		lastErr = err
	}
	return identity.KnownServer{}, lastErr
}

func newDiscoverCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List fleet daemons on the LAN",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			found, err := discovery.Browse(cmd.Context(), timeout)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(found) == 0 {
				fmt.Fprintln(out, "no fleet daemons found")
				return nil
			}
			store, err := client.OpenServers()
			if err != nil {
				return err
			}
			t := newTable(out, "NAME", "ID", "ADDRESS", "PAIRED")
			for _, f := range found {
				addr := "-"
				if as := foundAddrs(f); len(as) > 0 {
					addr = as[0]
				}
				_, paired := store.Find(f.ServerID)
				t.row(orDash(f.Name), orDash(shortID(f.ServerID)), addr, yesNo(paired && f.ServerID != ""))
			}
			t.flush()
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "how long to listen for answers")
	return cmd
}

func newServersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "servers",
		Short: "List remote daemons this machine is paired with",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := client.OpenServers()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			list := store.List()
			if len(list) == 0 {
				fmt.Fprintln(out, "no paired servers (pair with: fleet connect <addr> --code XXXX-XXXX)")
				return nil
			}
			t := newTable(out, "NAME", "ID", "ADDRESS", "PAIRED")
			for _, k := range list {
				t.row(orDash(k.Name), shortID(k.ID), orDash(k.Address), age(k.PairedAt.UnixMilli())+" ago")
			}
			t.flush()
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "rm <name|id>",
		Aliases: []string{"remove"},
		Short:   "Forget a paired server (revoke this device on the server with `fleet devices revoke`)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := client.OpenServers()
			if err != nil {
				return err
			}
			k, ok := client.FindServer(store, args[0])
			if !ok {
				return fmt.Errorf("unknown server %q", args[0])
			}
			if err := store.Remove(k.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "forgot %s (%s)\n", k.Name, shortID(k.ID))
			return nil
		},
	})
	return cmd
}

func newDevicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List devices paired with the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				devs, err := c.ListDevices(ctx)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(devs) == 0 {
					fmt.Fprintln(out, "no paired devices (pair one with: fleet pair)")
					return nil
				}
				t := newTable(out, "ID", "NAME", "PLATFORM", "PAIRED", "LAST SEEN", "CONNECTED")
				for _, d := range devs {
					seen := "-"
					if d.LastSeenAtMs > 0 {
						seen = age(d.LastSeenAtMs) + " ago"
					}
					t.row(shortID(d.Id), orDash(d.Name), orDash(d.Platform), age(d.PairedAtMs)+" ago", seen, yesNo(d.Connected))
				}
				t.flush()
				return nil
			})
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <id|name>",
		Short: "Unpair a device; it must pair again to reconnect",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				if err := c.RevokeDevice(ctx, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
				return nil
			})
		},
	})
	return cmd
}
