package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"fleet/internal/adapter"
)

// usageTTL is how long an adapter's usage is reused: every open dashboard,
// here or on another machine of the fleet, asks every minute, and the
// providers rate limit the endpoint (Anthropic answers 429 often).
const usageTTL = 5 * time.Minute

// usageRetry is how long to wait after a failed request before asking
// again; it doubles with every failure in a row, up to usageRetryMax.
const (
	usageRetry    = time.Minute
	usageRetryMax = 30 * time.Minute
)

// agentUsage is an adapter's CLI on this machine and its account's usage.
type agentUsage struct {
	adapter adapter.Adapter
	det     adapter.Detection
	usage   *adapter.Usage
	// err is why usage is unknown, or why it could not be renewed.
	err error
	// at is when usage was told; zero if it never was.
	at time.Time
}

// usageCache keeps the last usage each provider told, also across daemon
// restarts (in path), and shows it until a newer one arrives.
type usageCache struct {
	// path is the file the cache is kept in; "" keeps it in memory only.
	path string

	mu     sync.Mutex
	loaded bool
	last   map[string]usageEntry
	// wait serializes requests per adapter, so a burst asks once.
	wait map[string]*sync.Mutex
}

type usageEntry struct {
	// usage is the last one told; nil if none was.
	usage *adapter.Usage
	// at is when usage was told.
	at time.Time
	// err is why the last request failed; nil if it did not.
	err error
	// next is when to ask again after a failure, fails how many failed in
	// a row.
	next  time.Time
	fails int
}

// usageFile is the JSON of the cache file: each adapter's last usage.
type usageFile map[string]struct {
	Usage *adapter.Usage `json:"usage"`
	At    time.Time      `json:"at"`
}

func (c *usageCache) lock(id string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loaded = true
		c.wait = map[string]*sync.Mutex{}
		c.last = map[string]usageEntry{}
		c.load()
	}
	m := c.wait[id]
	if m == nil {
		m = &sync.Mutex{}
		c.wait[id] = m
	}
	return m
}

// load reads the cache file; c.mu is held.
func (c *usageCache) load() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f usageFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	for id, e := range f {
		if e.Usage != nil {
			c.last[id] = usageEntry{usage: e.Usage, at: e.At}
		}
	}
}

// save writes the cache file; c.mu is held.
func (c *usageCache) save() {
	if c.path == "" {
		return
	}
	f := usageFile{}
	for id, e := range c.last {
		if e.usage != nil {
			f[id] = struct {
				Usage *adapter.Usage `json:"usage"`
				At    time.Time      `json:"at"`
			}{e.usage, e.at}
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

// get returns u's usage, asking the provider if the cached one is stale.
// A failed request keeps the last usage, with err saying why it is old.
func (c *usageCache) get(ctx context.Context, id string, u adapter.Usager) usageEntry {
	m := c.lock(id)
	m.Lock()
	defer m.Unlock()
	c.mu.Lock()
	e := c.last[id]
	c.mu.Unlock()
	now := time.Now()
	if e.err == nil && e.usage != nil && now.Sub(e.at) < usageTTL || e.err != nil && now.Before(e.next) {
		return e.served(now)
	}
	usage, err := u.Usage(ctx)
	if ctx.Err() != nil {
		return e.served(now)
	}
	if err == nil {
		e = usageEntry{usage: usage, at: now}
	} else {
		e.err = err
		e.next = now.Add(min(usageRetry<<e.fails, usageRetryMax))
		e.fails = min(e.fails+1, 10)
	}
	c.mu.Lock()
	c.last[id] = e
	if err == nil {
		c.save()
	}
	c.mu.Unlock()
	return e.served(now)
}

// served is e as shown at now: windows that have reset since the usage was
// told are shown unused.
func (e usageEntry) served(now time.Time) usageEntry {
	if e.usage == nil {
		return e
	}
	u := *e.usage
	u.Limits = make([]adapter.UsageLimit, len(e.usage.Limits))
	for i, l := range e.usage.Limits {
		if !l.ResetsAt.IsZero() && !l.ResetsAt.After(now) {
			l.Percent, l.ResetsAt = 0, time.Time{}
		}
		u.Limits[i] = l
	}
	e.usage = &u
	return e
}

// agentUsages detects every adapter's CLI and asks the available ones that
// can tell for their account's usage, all at once.
func (d *daemon) agentUsages(ctx context.Context) []agentUsage {
	all := d.opts.Adapters.All()
	out := make([]agentUsage, len(all))
	var wg sync.WaitGroup
	for i, a := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := agentUsage{adapter: a, det: a.Detect(ctx)}
			if u, ok := a.(adapter.Usager); ok && r.det.Available {
				e := d.usage.get(ctx, a.ID(), u)
				r.usage, r.err, r.at = e.usage, e.err, e.at
			}
			out[i] = r
		}()
	}
	wg.Wait()
	return out
}
