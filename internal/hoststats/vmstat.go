package hoststats

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// vmPage is vm_stat's header: "Mach Virtual Memory Statistics: (page size
// of 16384 bytes)".
var vmPage = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat sums the free, speculative and inactive pages of vm_stat's
// output, in bytes (macOS). vm_stat's "free" leaves out the speculative
// pages the kernel counts as free; purgeable pages are among the others.
func parseVMStat(text string) uint64 {
	m := vmPage.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	page, _ := strconv.ParseUint(m[1], 10, 64)
	var pages uint64
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "Pages free", "Pages speculative", "Pages inactive":
			n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64)
			pages += n
		}
	}
	return pages * page
}
