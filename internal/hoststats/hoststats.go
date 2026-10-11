// Package hoststats reads how the machine is doing: what it is (OS, CPU),
// how busy (load), and how full (memory, swap, disks). The daemon serves
// it to `fleet host` and the dashboard.
//
// Linux reads /proc and /etc/os-release; macOS asks sysctl and vm_stat.
// Both read disks with statfs. What cannot be read is left zero: a
// partial answer beats none.
package hoststats

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Stats is a reading of the machine.
type Stats struct {
	ReadAt   time.Time
	Hostname string
	// OSName is the OS release, e.g. "Ubuntu 24.04.1 LTS" or "macOS 15.2";
	// Kernel the kernel's, e.g. "6.8.0-139-generic".
	OSName string
	Kernel string
	// CPUModel is e.g. "Intel(R) Core(TM) i7-7700K CPU @ 4.20GHz"; CPUs
	// counts logical CPUs.
	CPUModel string
	CPUs     int
	BootedAt time.Time
	// Load is the load average over 1, 5 and 15 minutes.
	Load [3]float64
	// Memory and swap in bytes. MemAvailable is what programs can still
	// get without swapping.
	MemTotal     uint64
	MemAvailable uint64
	SwapTotal    uint64
	SwapUsed     uint64
	Disks        []Disk
}

// Disk is the space of a filesystem in bytes. Available is what users
// other than root may still write; Used plus Available can fall short of
// Total by the blocks reserved for root.
type Disk struct {
	Mount     string
	Total     uint64
	Used      uint64
	Available uint64
}

// readTimeout bounds the commands a reading runs (vm_stat on macOS).
const readTimeout = 3 * time.Second

// Read reads the machine. Disks are the filesystems dirs are on, each
// once, in the order of dirs; dirs that cannot be read are left out.
func Read(ctx context.Context, dirs []string) Stats {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	s := Stats{ReadAt: time.Now(), CPUs: runtime.NumCPU()}
	s.Hostname, _ = os.Hostname()
	readSystem(ctx, &s)
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(d); err == nil {
			d = real
		}
		disk, id, ok := diskOf(d)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		s.Disks = append(s.Disks, disk)
	}
	return s
}

// SystemDirs are the folders whose filesystems matter whatever the
// daemon serves: / where it is not a read-only system volume (macOS).
func SystemDirs() []string {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return []string{"/"}
}
