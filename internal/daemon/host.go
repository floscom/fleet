package daemon

import (
	"context"
	"os"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/hoststats"
)

// hostStats reads how this machine is doing. Its disks are the
// filesystems of /, the home folder, the roots, and fleet's own files and
// sandbox.
func (d *daemon) hostStats(ctx context.Context) *fleetv1.HostStats {
	cfg := d.config()
	dirs := hoststats.SystemDirs()
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, home)
	}
	for _, r := range cfg.Roots {
		dirs = append(dirs, r.Path)
	}
	dirs = append(dirs, config.Path(), cfg.Sandbox.DirOrDefault())
	s := hoststats.Read(ctx, dirs)
	out := &fleetv1.HostStats{
		ReadAtMs: s.ReadAt.UnixMilli(), Hostname: s.Hostname, OsName: s.OSName, Kernel: s.Kernel,
		CpuModel: s.CPUModel, Cpus: int32(s.CPUs),
		Load1: s.Load[0], Load5: s.Load[1], Load15: s.Load[2],
		MemoryTotal: s.MemTotal, MemoryAvailable: s.MemAvailable, SwapTotal: s.SwapTotal, SwapUsed: s.SwapUsed,
	}
	if !s.BootedAt.IsZero() {
		out.BootedAtMs = s.BootedAt.UnixMilli()
	}
	for _, disk := range s.Disks {
		out.Disks = append(out.Disks, &fleetv1.DiskStats{Mount: disk.Mount, Total: disk.Total, Used: disk.Used, Available: disk.Available})
	}
	return out
}
