package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

// When a host reading needs attention: a share of the memory, swap or a
// disk in use, or a load per CPU.
const (
	hostFullPct  = 90
	hostBusyLoad = 1.0
)

func newHostCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Show the daemon's machine: OS, CPU, load, memory and disks",
		Long: `host shows what the machine the daemon runs on is (OS, CPU, uptime) and
how it is doing: load, memory, swap, and the space left on the disks that
hold /, the home folder, the roots and fleet's own files. A line that
needs attention ends in "!": a load above one per CPU, or memory, swap or
a disk at least 90% full.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				s, err := c.GetHostStats(ctx)
				// The request has no arguments: a daemon that finds it
				// invalid does not know it.
				if client.Code(err) == fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
					return errors.New("this daemon is too old to tell its host stats: update it")
				}
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if asJSON {
					b, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true}.Marshal(s)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(out, "%s\n", b)
					return err
				}
				printHost(out, s)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// printHost prints a host reading, a line per resource.
func printHost(w io.Writer, s *fleetv1.HostStats) {
	head := []string{orDash(s.Hostname), s.OsName}
	if s.Kernel != "" {
		head = append(head, "kernel "+s.Kernel)
	}
	if s.BootedAtMs > 0 {
		head = append(head, "up "+age(s.BootedAtMs))
	}
	fmt.Fprintln(w, strings.Join(slices.DeleteFunc(head, func(s string) bool { return s == "" }), " · "))
	t := &table{w: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
	cpu := fmt.Sprintf("%d CPUs", s.Cpus)
	if s.CpuModel != "" {
		cpu = s.CpuModel + ", " + cpu
	}
	t.row("cpu", cpu)
	t.row("load", fmt.Sprintf("%.2f %.2f %.2f  (%s of %d CPUs)", s.Load1, s.Load5, s.Load15, pct(loadPct(s)), s.Cpus)+warn(loadBusy(s)))
	if s.MemoryTotal > 0 {
		used := s.MemoryTotal - s.MemoryAvailable
		p := share(used, s.MemoryTotal)
		t.row("memory", fmt.Sprintf("%s used of %s (%s), %s available", bytesIEC(used), bytesIEC(s.MemoryTotal), pct(p), bytesIEC(s.MemoryAvailable))+warn(p >= hostFullPct))
	}
	if s.SwapTotal > 0 {
		p := share(s.SwapUsed, s.SwapTotal)
		t.row("swap", fmt.Sprintf("%s used of %s (%s)", bytesIEC(s.SwapUsed), bytesIEC(s.SwapTotal), pct(p))+warn(p >= hostFullPct))
	}
	for i, d := range s.Disks {
		label := ""
		if i == 0 {
			label = "disk"
		}
		p := diskPct(d)
		t.row(label, d.Mount, fmt.Sprintf("%s used of %s (%s), %s free", bytesIEC(d.Used), bytesIEC(d.Total), pct(p), bytesIEC(d.Available))+warn(p >= hostFullPct))
	}
	t.flush()
}

// hostSummary sums a host reading up in one line, for `fleet status`.
func hostSummary(s *fleetv1.HostStats) string {
	mark := func(on bool) string { return strings.Replace(warn(on), "  ", " ", 1) }
	parts := []string{fmt.Sprintf("load %.2f on %d CPUs", s.Load1, s.Cpus) + mark(loadBusy(s))}
	if s.MemoryTotal > 0 {
		p := share(s.MemoryTotal-s.MemoryAvailable, s.MemoryTotal)
		parts = append(parts, "memory "+pct(p)+mark(p >= hostFullPct))
	}
	for _, d := range s.Disks {
		p := diskPct(d)
		parts = append(parts, "disk "+d.Mount+" "+pct(p)+mark(p >= hostFullPct))
	}
	return strings.Join(parts, " · ")
}

// loadPct is the 1-minute load as a share of the CPUs.
func loadPct(s *fleetv1.HostStats) float64 {
	if s.Cpus <= 0 {
		return 0
	}
	return 100 * s.Load1 / float64(s.Cpus)
}

func loadBusy(s *fleetv1.HostStats) bool {
	return s.Cpus > 0 && s.Load1/float64(s.Cpus) > hostBusyLoad
}

// diskPct is how full a disk is, as df counts it: used of what users may
// fill (used plus available).
func diskPct(d *fleetv1.DiskStats) float64 {
	return share(d.Used, d.Used+d.Available)
}

func share(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

func pct(p float64) string { return fmt.Sprintf("%.0f%%", math.Floor(p+0.5)) }

func warn(on bool) string {
	if on {
		return "  !"
	}
	return ""
}

// bytesIEC renders n bytes in binary units: 512 B, 3.4 KiB, 12.0 GiB.
func bytesIEC(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v, unit := float64(n)/1024, 0
	for v >= 1024 && unit < 4 {
		v /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTP"[unit])
}
