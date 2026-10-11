package hoststats

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func TestParseVMStat(t *testing.T) {
	out := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10000.
Pages active:                            200000.
Pages inactive:                           50000.
Pages speculative:                         2000.
Pages throttled:                              0.
Pages wired down:                         90000.
Pages purgeable:                           3000.
`
	if got, want := parseVMStat(out), uint64(62000*16384); got != want {
		t.Errorf("parseVMStat = %d, want %d", got, want)
	}
	if got := parseVMStat("Pages free: 5."); got != 0 {
		t.Errorf("without a page size: %d", got)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	s := Read(context.Background(), append(SystemDirs(), dir, dir, "/does/not/exist", ""))
	if s.ReadAt.IsZero() || s.CPUs != runtime.NumCPU() || s.Hostname == "" || s.OSName == "" {
		t.Errorf("stats = %+v", s)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	if s.MemTotal == 0 || s.MemAvailable > s.MemTotal || s.BootedAt.IsZero() || s.Kernel == "" {
		t.Errorf("stats = %+v", s)
	}
	// The temp dir counts once, next to / if it is on another filesystem.
	if n := len(s.Disks); n == 0 || n > len(SystemDirs())+1 {
		t.Fatalf("disks = %+v", s.Disks)
	}
	for _, d := range s.Disks {
		if d.Mount == "" || d.Total == 0 || d.Used > d.Total || d.Available > d.Total {
			t.Errorf("disk = %+v", d)
		}
	}
	if _, err := os.Stat(s.Disks[len(s.Disks)-1].Mount); err != nil {
		t.Errorf("mount: %v", err)
	}
}
