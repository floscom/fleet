//go:build linux

package hoststats

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func readSystem(_ context.Context, s *Stats) {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join("/proc", name))
		return string(b)
	}
	s.Load = parseLoadavg(read("loadavg"))
	s.MemTotal, s.MemAvailable, s.SwapTotal, s.SwapUsed = parseMeminfo(read("meminfo"))
	if up := parseUptime(read("uptime")); up > 0 {
		s.BootedAt = s.ReadAt.Add(-up).Truncate(time.Second)
	}
	s.CPUModel = parseCPUInfo(read("cpuinfo"))
	s.Kernel = strings.TrimSpace(read("sys/kernel/osrelease"))
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		s.OSName = parseOSRelease(string(b))
	}
	if s.OSName == "" {
		s.OSName = "Linux"
	}
}

// parseLoadavg reads /proc/loadavg: "1.77 1.48 1.43 1/3823 1511619".
func parseLoadavg(text string) (load [3]float64) {
	f := strings.Fields(text)
	for i := 0; i < 3 && i < len(f); i++ {
		load[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return load
}

// parseMeminfo reads /proc/meminfo ("MemTotal:   32734164 kB" a line).
// Kernels before 3.14 have no MemAvailable: free memory, buffers and page
// cache stand in.
func parseMeminfo(text string) (total, avail, swapTotal, swapUsed uint64) {
	kb := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			kb[name] = n << 10
		}
	}
	total = kb["MemTotal"]
	avail, ok := kb["MemAvailable"]
	if !ok {
		avail = kb["MemFree"] + kb["Buffers"] + kb["Cached"]
	}
	swapTotal = kb["SwapTotal"]
	if free := kb["SwapFree"]; free <= swapTotal {
		swapUsed = swapTotal - free
	}
	return total, min(avail, total), swapTotal, swapUsed
}

// parseUptime reads /proc/uptime: seconds since boot, then idle seconds.
func parseUptime(text string) time.Duration {
	f := strings.Fields(text)
	if len(f) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil || sec <= 0 {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

// parseCPUInfo finds the CPU's name in /proc/cpuinfo: "model name" on x86,
// "Model" or "Hardware" on some ARM boards ("" when none says).
func parseCPUInfo(text string) string {
	found := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), ":")
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		if ok && val != "" && found[name] == "" {
			found[name] = val
		}
	}
	for _, k := range []string{"model name", "Model", "Hardware", "cpu model"} {
		if v := found[k]; v != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// parseOSRelease reads PRETTY_NAME (else NAME and VERSION) from
// /etc/os-release.
func parseOSRelease(text string) string {
	vals := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		vals[k] = v
	}
	if v := vals["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(vals["NAME"] + " " + vals["VERSION"])
}

// diskOf reads the space of the filesystem dir is on; id tells
// filesystems apart.
func diskOf(dir string) (d Disk, id string, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Disk{}, "", false
	}
	dev, ok := devOf(dir)
	if !ok {
		return Disk{}, "", false
	}
	bs := uint64(st.Frsize)
	if bs == 0 {
		bs = uint64(st.Bsize)
	}
	d = Disk{Mount: mountPoint(dir, dev), Total: st.Blocks * bs, Available: st.Bavail * bs}
	if st.Bfree <= st.Blocks {
		d.Used = (st.Blocks - st.Bfree) * bs
	}
	return d, strconv.FormatUint(dev, 10), true
}

func devOf(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true // its type differs between architectures
}

// mountPoint is the topmost folder above dir on the same device.
func mountPoint(dir string, dev uint64) string {
	p := filepath.Clean(dir)
	for p != "/" {
		parent := filepath.Dir(p)
		if d, ok := devOf(parent); !ok || d != dev {
			break
		}
		p = parent
	}
	return p
}
