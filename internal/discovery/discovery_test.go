package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func TestFromEntry(t *testing.T) {
	e := zeroconf.NewServiceEntry(`My\ Mac`, Service, "local.")
	e.HostName = "mac.local."
	e.Port = 7420
	e.Text = []string{"v=1", "id=abc", "name=My Mac"}
	e.AddrIPv6 = []net.IP{net.ParseIP("fd00::1"), net.ParseIP("fe80::1")}
	e.AddrIPv4 = []net.IP{net.IPv4(192, 168, 1, 5)}
	f, ok := fromEntry(e)
	if !ok {
		t.Fatal("entry rejected")
	}
	want := Found{Instance: "My Mac", ServerID: "abc", Name: "My Mac", Host: "mac.local",
		Addrs: []string{"192.168.1.5", "fd00::1"}, Port: 7420}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got %+v\nwant %+v", f, want)
	}

	e.Text = []string{"v=2", "id=abc"}
	if _, ok := fromEntry(e); ok {
		t.Fatal("future protocol version accepted")
	}
	e.Text = []string{"id=abc"}
	if f, _ := fromEntry(e); f.Name != "My Mac" || f.WebPort != 0 {
		t.Fatalf("name fallback = %q, web port %d", f.Name, f.WebPort)
	}
	for txt, want := range map[string]int{"web=7421": 7421, "web=0": 0, "web=70000": 0, "web=x": 0} {
		e.Text = []string{"v=1", txt}
		if f, _ := fromEntry(e); f.WebPort != want {
			t.Errorf("%s: web port %d, want %d", txt, f.WebPort, want)
		}
	}
}

func TestMergeAddrs(t *testing.T) {
	got := mergeAddrs([]string{"fd00::1", "10.0.0.1"}, []string{"10.0.0.1", "10.0.0.2", "fd00::2"})
	want := []string{"10.0.0.1", "10.0.0.2", "fd00::1", "fd00::2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 2); got != "h" {
		t.Fatalf("truncate split a rune: %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestAdvertiseInvalidPort(t *testing.T) {
	if _, err := Advertise(context.Background(), Advertisement{Name: "x", Port: 0}); err == nil {
		t.Fatal("port 0 accepted")
	}
}

// TestAdvertiseBrowseLoopback needs working multicast; it skips otherwise.
func TestAdvertiseBrowseLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS loopback test skipped in -short mode")
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	name := "fleet-test-" + id[:8]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := Advertise(ctx, Advertisement{Instance: name, ServerID: id, Name: name, Port: 7499, WebPort: 7498})
	if err != nil {
		t.Skipf("multicast unavailable: %v", err)
	}
	defer stop()

	var found *Found
	for attempt := 0; attempt < 3 && found == nil; attempt++ {
		list, err := Browse(context.Background(), 2*time.Second)
		if err != nil {
			t.Skipf("multicast unavailable: %v", err)
		}
		seen := map[string]bool{}
		for i := range list {
			if seen[list[i].ServerID] && list[i].ServerID != "" {
				t.Fatalf("duplicate server id %s", list[i].ServerID)
			}
			seen[list[i].ServerID] = true
			if list[i].ServerID == id {
				found = &list[i]
			}
		}
	}
	if found == nil {
		t.Skip("own advertisement not seen; multicast loopback likely unavailable")
	}
	if found.Name != name || found.Port != 7499 || found.WebPort != 7498 || len(found.Addrs) == 0 {
		t.Fatalf("unexpected result: %+v", *found)
	}
	stop()
	stop() // idempotent
}
