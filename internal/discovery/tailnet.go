package discovery

// Finding daemons on the machines of this machine's tailnet. Tailscale
// carries no multicast, so mDNS sees nothing across it: instead the
// tailscale CLI lists the online machines, and each is asked for HelloPath
// on the dashboard ports. Like mDNS TXT records, a hello is unauthenticated.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HelloPath is the open dashboard route that describes a daemon (Hello).
const HelloPath = "/api/hello"

// Hello is the reply to HelloPath.
type Hello struct {
	// V is the protocol version, as the "v" TXT record.
	V    int    `json:"v"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Port is the TLS listen port, 0 when off.
	Port int `json:"port"`
	// Web is the dashboard's port.
	Web int `json:"web"`
}

// tailscaleBins are tried in order: the CLI on PATH, then the one inside
// the macOS app (which does not install it on PATH by default).
var tailscaleBins = []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}

// maxProbes caps concurrent hello requests.
const maxProbes = 16

// tailnetPeer is a machine of the tailnet.
type tailnetPeer struct {
	Host  string
	Addrs []string
}

// BrowseTailnet asks every online machine of the tailnet for a daemon
// whose dashboard listens on one of webPorts, within timeout. Without
// tailscale, or when it is not running, it finds nothing and no error.
func BrowseTailnet(ctx context.Context, timeout time.Duration, webPorts []int) ([]Found, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	peers, err := tailnetPeers(ctx)
	if err != nil || len(peers) == 0 {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: timeout / 2}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var (
		mu  sync.Mutex
		out []Found
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, maxProbes)
	for _, p := range peers {
		for _, port := range webPorts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if f, ok := probe(ctx, client, p, port); ok {
					mu.Lock()
					out = append(out, f)
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err // caller cancelled
	}
	return Merge(out), nil
}

// probe asks p for a hello on port, at its first address (Tailscale
// routes every one of them alike).
func probe(ctx context.Context, client *http.Client, p tailnetPeer, port int) (Found, bool) {
	url := "http://" + net.JoinHostPort(p.Addrs[0], strconv.Itoa(port)) + HelloPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Found{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return Found{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Found{}, false
	}
	var h Hello
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil || h.V != 1 || h.ID == "" {
		return Found{}, false
	}
	name := truncate(h.Name, maxTXTValue)
	if name == "" {
		name = p.Host
	}
	return Found{
		Instance: name, ServerID: h.ID, Name: name, Host: p.Host,
		Addrs: p.Addrs, Port: h.Port, WebPort: port,
	}, true
}

// tailnetPeers lists the online machines of the tailnet, this one aside.
func tailnetPeers(ctx context.Context) ([]tailnetPeer, error) {
	var out []byte
	for _, bin := range tailscaleBins {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		if out, err = exec.CommandContext(ctx, path, "status", "--json").Output(); err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, nil // timed out: nothing found this time
		}
		out = nil // not logged in, daemon down, ...
	}
	if out == nil {
		return nil, nil
	}
	return parseStatus(out), nil
}

// parseStatus reads `tailscale status --json`.
func parseStatus(b []byte) []tailnetPeer {
	var st struct {
		BackendState string
		Peer         map[string]struct {
			HostName     string
			DNSName      string
			TailscaleIPs []string
			Online       bool
		}
	}
	if json.Unmarshal(b, &st) != nil || st.BackendState != "Running" {
		return nil
	}
	var out []tailnetPeer
	for _, p := range st.Peer {
		addrs := mergeAddrs(nil, p.TailscaleIPs)
		if !p.Online || len(addrs) == 0 {
			continue
		}
		host := strings.TrimSuffix(p.DNSName, ".")
		if host == "" {
			host = p.HostName
		}
		out = append(out, tailnetPeer{Host: host, Addrs: addrs})
	}
	return out
}

// BrowseAll finds daemons via mDNS and on the tailnet at once (see
// BrowseTailnet for webPorts). mDNS errors are returned only when the
// tailnet found nothing either.
func BrowseAll(ctx context.Context, timeout time.Duration, webPorts []int) ([]Found, error) {
	var (
		lan, tn []Found
		lanErr  error
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tn, _ = BrowseTailnet(ctx, timeout, webPorts)
	}()
	lan, lanErr = Browse(ctx, timeout)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lanErr != nil && len(tn) == 0 {
		return nil, lanErr
	}
	return Merge(lan, tn), nil
}

// Merge unions lists of found daemons by ServerID (entries without one by
// instance name), keeping the first-seen fields and uniting addresses in
// first-seen order, so LAN addresses given first are tried first.
func Merge(lists ...[]Found) []Found {
	var out []Found
	at := map[string]int{}
	for _, l := range lists {
		for _, f := range l {
			key := f.ServerID
			if key == "" {
				key = "instance:" + f.Instance
			}
			if i, dup := at[key]; dup {
				out[i].Addrs = mergeAddrs(out[i].Addrs, f.Addrs)
				if out[i].WebPort == 0 {
					out[i].WebPort = f.WebPort
				}
				continue
			}
			at[key] = len(out)
			f.Addrs = append([]string(nil), f.Addrs...)
			out = append(out, f)
		}
	}
	return out
}
