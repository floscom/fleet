//go:build darwin

package hoststats

import (
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func readSystem(ctx context.Context, s *Stats) {
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }: three uint32, then
	// (aligned to 8) an int64.
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		if scale := float64(binary.LittleEndian.Uint64(b[16:])); scale > 0 {
			for i := range s.Load {
				s.Load[i] = float64(binary.LittleEndian.Uint32(b[4*i:])) / scale
			}
		}
	}
	s.MemTotal, _ = unix.SysctlUint64("hw.memsize")
	s.MemAvailable = min(vmAvailable(ctx), s.MemTotal)
	// struct xsw_usage { u_int64_t xsu_total, xsu_avail, xsu_used; ... }
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		s.SwapTotal = binary.LittleEndian.Uint64(b[0:])
		s.SwapUsed = binary.LittleEndian.Uint64(b[16:])
	}
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil && tv.Sec > 0 {
		s.BootedAt = time.Unix(tv.Sec, 0)
	}
	s.CPUModel, _ = unix.Sysctl("machdep.cpu.brand_string")
	s.Kernel, _ = unix.Sysctl("kern.osrelease")
	s.OSName = "macOS"
	if v, err := unix.Sysctl("kern.osproductversion"); err == nil && v != "" {
		s.OSName += " " + v
	}
}

// vmAvailable is the memory macOS can hand out without swapping, as
// vm_stat counts it.
func vmAvailable(ctx context.Context) uint64 {
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return 0
	}
	return parseVMStat(string(out))
}

// diskOf reads the space of the filesystem dir is on; id tells
// filesystems apart.
func diskOf(dir string) (d Disk, id string, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return Disk{}, "", false
	}
	bs := uint64(st.Bsize)
	d = Disk{Mount: unix.ByteSliceToString(st.Mntonname[:]), Total: st.Blocks * bs, Available: st.Bavail * bs}
	if st.Bfree <= st.Blocks {
		d.Used = (st.Blocks - st.Bfree) * bs
	}
	if d.Mount == "" {
		d.Mount = dir
	}
	return d, fmt.Sprint(st.Fsid.Val), true
}
