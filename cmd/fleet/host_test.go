package main

import (
	"strings"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func TestPrintHost(t *testing.T) {
	const gib = 1 << 30
	s := &fleetv1.HostStats{
		Hostname: "zerox", OsName: "Ubuntu 24.04.5 LTS", Kernel: "6.8.0", CpuModel: "Intel i7", Cpus: 8,
		Load1: 9.5, Load5: 1, Load15: 0.5,
		MemoryTotal: 32 * gib, MemoryAvailable: 20 * gib, SwapTotal: 8 * gib, SwapUsed: 5 * gib,
		Disks: []*fleetv1.DiskStats{
			{Mount: "/", Total: 455 * gib, Used: 422 * gib, Available: 14 * gib},
			{Mount: "/mnt/usb", Total: 30 * gib, Used: 1 * gib, Available: 29 * gib},
		},
	}
	var b strings.Builder
	printHost(&b, s)
	want := `zerox · Ubuntu 24.04.5 LTS · kernel 6.8.0
cpu     Intel i7, 8 CPUs
load    9.50 1.00 0.50  (119% of 8 CPUs)  !
memory  12.0 GiB used of 32.0 GiB (38%), 20.0 GiB available
swap    5.0 GiB used of 8.0 GiB (63%)
disk    /         422.0 GiB used of 455.0 GiB (97%), 14.0 GiB free  !
        /mnt/usb  1.0 GiB used of 30.0 GiB (3%), 29.0 GiB free
`
	if b.String() != want {
		t.Errorf("printHost:\n%s\nwant:\n%s", b.String(), want)
	}
	if got := hostSummary(s); got != "load 9.50 on 8 CPUs ! · memory 38% · disk / 97% ! · disk /mnt/usb 3%" {
		t.Errorf("hostSummary = %q", got)
	}
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 * gib: "5.0 GiB"} {
		if got := bytesIEC(n); got != want {
			t.Errorf("bytesIEC(%d) = %q, want %q", n, got, want)
		}
	}
}
