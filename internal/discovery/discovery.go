// Package discovery advertises and finds fleet daemons on the LAN via mDNS
// (DNS-SD service type "_fleet._tcp", domain "local.").
//
// TXT records: "v=1", "id=<server id>" (full hex SHA-256 of the daemon cert),
// "name=<server name>".
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// domain is the mDNS domain.
const domain = "local."

// maxTXTValue keeps each TXT string within the 255-byte limit.
const maxTXTValue = 200

// Service is the DNS-SD service type.
const Service = "_fleet._tcp"

// Advertisement describes this daemon.
type Advertisement struct {
	// Instance is the DNS-SD instance name (usually the server name).
	Instance string
	ServerID string
	Name     string
	Port     int
}

// Advertise registers the service until ctx is cancelled or stop is called.
func Advertise(ctx context.Context, a Advertisement) (stop func(), err error) {
	if a.Port <= 0 || a.Port > 65535 {
		return nil, fmt.Errorf("discovery: invalid port %d", a.Port)
	}
	instance := a.Instance
	if instance == "" {
		instance = a.Name
	}
	if instance == "" {
		instance = "fleet"
	}
	txt := []string{"v=1", "id=" + a.ServerID, "name=" + truncate(a.Name, maxTXTValue)}
	srv, err := zeroconf.Register(truncate(instance, 63), Service, domain, a.Port, txt, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: register: %w", err)
	}
	var once sync.Once
	done := make(chan struct{})
	stop = func() {
		once.Do(func() {
			close(done)
			srv.Shutdown()
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

// Found is a discovered daemon.
type Found struct {
	Instance string
	ServerID string
	Name     string
	Host     string
	// Addrs are IPs (IPv4 first) to try in order.
	Addrs []string
	Port  int
}

// Browse collects daemons answering within timeout, de-duplicated by ServerID.
// Entries without an id are keyed by instance name.
func Browse(ctx context.Context, timeout time.Duration) ([]Found, error) {
	res, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	if err := res.Browse(ctx, Service, domain, entries); err != nil {
		return nil, fmt.Errorf("discovery: browse: %w", err)
	}
	byID := map[string]*Found{}
	// Drain until zeroconf closes the channel (on ctx expiry); it blocks
	// sending otherwise and would leak its goroutines.
	for e := range entries {
		f, ok := fromEntry(e)
		if !ok {
			continue
		}
		key := f.ServerID
		if key == "" {
			key = "instance:" + f.Instance
		}
		if prev, dup := byID[key]; dup {
			prev.Addrs = mergeAddrs(prev.Addrs, f.Addrs)
			continue
		}
		byID[key] = &f
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err // caller cancelled
	}
	out := make([]Found, 0, len(byID))
	for _, f := range byID {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ServerID < out[j].ServerID
	})
	return out, nil
}

// fromEntry converts a zeroconf entry, rejecting other protocol versions.
func fromEntry(e *zeroconf.ServiceEntry) (Found, bool) {
	f := Found{
		Instance: unescapeInstance(e.Instance),
		Host:     strings.TrimSuffix(e.HostName, "."),
		Port:     e.Port,
	}
	for _, kv := range e.Text {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "v":
			if v != "1" {
				return Found{}, false
			}
		case "id":
			f.ServerID = v
		case "name":
			f.Name = v
		}
	}
	if f.Name == "" {
		f.Name = f.Instance
	}
	var ips []net.IP
	ips = append(ips, e.AddrIPv4...)
	ips = append(ips, e.AddrIPv6...)
	f.Addrs = mergeAddrs(nil, addrStrings(ips))
	return f, f.Port > 0
}

// addrStrings formats IPs, dropping unusable ones (IPv6 link-local needs a
// zone that mDNS answers do not carry).
func addrStrings(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		if ip == nil || ip.IsUnspecified() || (ip.To4() == nil && ip.IsLinkLocalUnicast()) {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// mergeAddrs unions a and b, de-duplicated, IPv4 first, otherwise keeping
// first-seen order.
func mergeAddrs(a, b []string) []string {
	seen := map[string]bool{}
	var v4, v6 []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			v4 = append(v4, s)
		} else {
			v6 = append(v6, s)
		}
	}
	return append(v4, v6...)
}

// unescapeInstance undoes DNS label escaping ("My\ Mac" -> "My Mac").
func unescapeInstance(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
