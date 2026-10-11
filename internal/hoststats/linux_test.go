//go:build linux

package hoststats

import (
	"testing"
	"time"
)

func TestParseLinux(t *testing.T) {
	if got := parseLoadavg("1.77 1.48 1.43 1/3823 1511619\n"); got != [3]float64{1.77, 1.48, 1.43} {
		t.Errorf("loadavg = %v", got)
	}
	if got := parseLoadavg(""); got != [3]float64{} {
		t.Errorf("empty loadavg = %v", got)
	}

	total, avail, swap, used := parseMeminfo("MemTotal:       32734164 kB\nMemFree:          800000 kB\nMemAvailable:   20000000 kB\nBuffers: 10 kB\nSwapTotal:       8388604 kB\nSwapFree:        4000000 kB\n")
	if total != 32734164<<10 || avail != 20000000<<10 || swap != 8388604<<10 || used != (8388604-4000000)<<10 {
		t.Errorf("meminfo = %d %d %d %d", total, avail, swap, used)
	}
	// Before Linux 3.14: free, buffers and cache.
	if _, avail, _, used := parseMeminfo("MemTotal: 100 kB\nMemFree: 10 kB\nBuffers: 5 kB\nCached: 20 kB\n"); avail != 35<<10 || used != 0 {
		t.Errorf("old meminfo = %d %d", avail, used)
	}

	if got := parseUptime("1505342.17 9000000.00\n"); got != 1505342170*time.Millisecond {
		t.Errorf("uptime = %v", got)
	}
	if got := parseUptime("x"); got != 0 {
		t.Errorf("bad uptime = %v", got)
	}

	for text, want := range map[string]string{
		"processor\t: 0\nmodel name\t: Intel(R) Core(TM) i7-7700K CPU @ 4.20GHz\nprocessor\t: 1\nmodel name\t: other\n": "Intel(R) Core(TM) i7-7700K CPU @ 4.20GHz",
		"processor\t: 0\nBogoMIPS\t: 108.00\n\nHardware\t: BCM2835\nModel\t\t: Raspberry Pi 4 Model B Rev 1.4\n":        "Raspberry Pi 4 Model B Rev 1.4",
		"processor\t: 0\nBogoMIPS\t: 50.00\n": "",
	} {
		if got := parseCPUInfo(text); got != want {
			t.Errorf("cpuinfo %q = %q", text, got)
		}
	}

	for text, want := range map[string]string{
		"NAME=\"Ubuntu\"\nVERSION=\"24.04.1 LTS (Noble Numbat)\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n": "Ubuntu 24.04.1 LTS",
		"NAME=Alpine\nVERSION='3.20'\n": "Alpine 3.20",
		"":                              "",
	} {
		if got := parseOSRelease(text); got != want {
			t.Errorf("os-release %q = %q", text, got)
		}
	}
}
