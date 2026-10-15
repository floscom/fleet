package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
)

func TestParseStatus(t *testing.T) {
	status := `{"BackendState":"Running","Self":{"HostName":"me","TailscaleIPs":["100.0.0.1"],"Online":true},
	"Peer":{
	  "k1":{"HostName":"zerox","DNSName":"zerox.tail.ts.net.","TailscaleIPs":["fd7a::1","100.0.0.2"],"Online":true},
	  "k2":{"HostName":"air","DNSName":"air.tail.ts.net.","TailscaleIPs":["100.0.0.3"],"Online":false},
	  "k3":{"HostName":"bare","TailscaleIPs":["100.0.0.4"],"Online":true},
	  "k4":{"HostName":"noip","DNSName":"noip.tail.ts.net.","Online":true}}}`
	got := map[string][]string{}
	for _, p := range parseStatus([]byte(status)) {
		got[p.Host] = p.Addrs
	}
	want := map[string][]string{"zerox.tail.ts.net": {"100.0.0.2", "fd7a::1"}, "bare": {"100.0.0.4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if p := parseStatus([]byte(`{"BackendState":"Stopped","Peer":{"k":{"HostName":"x","TailscaleIPs":["100.0.0.2"],"Online":true}}}`)); p != nil {
		t.Fatalf("stopped tailscale: %v", p)
	}
	if p := parseStatus([]byte("not json")); p != nil {
		t.Fatalf("bad json: %v", p)
	}
}

func TestProbe(t *testing.T) {
	reply := `{"v":1,"id":"abc","name":"zerox","port":7420,"web":7421}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HelloPath {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(reply))
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	h, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	peer := tailnetPeer{Host: "zerox.tail.ts.net", Addrs: []string{h, "fd7a::1"}}
	f, ok := probe(context.Background(), ts.Client(), peer, port)
	want := Found{Instance: "zerox", ServerID: "abc", Name: "zerox", Host: "zerox.tail.ts.net",
		Addrs: []string{h, "fd7a::1"}, Port: 7420, WebPort: port}
	if !ok || !reflect.DeepEqual(f, want) {
		t.Fatalf("got %+v %v\nwant %+v", f, ok, want)
	}
	for _, r := range []string{`{"v":2,"id":"abc"}`, `{"v":1}`, `nope`} {
		reply = r
		if f, ok := probe(context.Background(), ts.Client(), peer, port); ok {
			t.Errorf("reply %s accepted: %+v", r, f)
		}
	}
	reply = `{"v":1,"id":"abc"}`
	if f, _ := probe(context.Background(), ts.Client(), peer, port); f.Name != "zerox.tail.ts.net" {
		t.Errorf("name fallback = %q", f.Name)
	}
	if _, ok := probe(context.Background(), ts.Client(), peer, port+1); ok && port < 65535 {
		t.Error("closed port accepted")
	}
}

func TestMerge(t *testing.T) {
	lan := []Found{{ServerID: "a", Name: "a", Addrs: []string{"192.168.1.5"}, WebPort: 0}, {Instance: "x", Addrs: []string{"192.168.1.6"}}}
	tn := []Found{{ServerID: "a", Name: "a-tn", Addrs: []string{"100.0.0.2", "192.168.1.5"}, WebPort: 7421}, {ServerID: "b", Addrs: []string{"100.0.0.3"}}}
	got := Merge(lan, tn)
	want := []Found{
		{ServerID: "a", Name: "a", Addrs: []string{"192.168.1.5", "100.0.0.2"}, WebPort: 7421},
		{Instance: "x", Addrs: []string{"192.168.1.6"}},
		{ServerID: "b", Addrs: []string{"100.0.0.3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if lan[0].Addrs[0] != "192.168.1.5" || len(lan[0].Addrs) != 1 {
		t.Fatalf("input changed: %+v", lan[0])
	}
}
