package daemon

import (
	"context"
	"sync"
	"time"

	"fleet/internal/adapter"
)

// usageTTL is how long an adapter's usage is reused: every open dashboard,
// here or on another machine of the fleet, asks every minute, and the
// providers need not hear about each.
const usageTTL = 45 * time.Second

// agentUsage is an adapter's CLI on this machine and its account's usage.
type agentUsage struct {
	adapter adapter.Adapter
	det     adapter.Detection
	usage   *adapter.Usage
	// err is why usage is unknown.
	err error
	// at is when usage was asked; zero if it was not (no Usager, no CLI).
	at time.Time
}

type usageCache struct {
	mu   sync.Mutex
	last map[string]usageEntry
	// wait serializes requests per adapter, so a burst asks once.
	wait map[string]*sync.Mutex
}

type usageEntry struct {
	usage *adapter.Usage
	err   error
	at    time.Time
}

func (c *usageCache) lock(id string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wait == nil {
		c.wait = map[string]*sync.Mutex{}
		c.last = map[string]usageEntry{}
	}
	m := c.wait[id]
	if m == nil {
		m = &sync.Mutex{}
		c.wait[id] = m
	}
	return m
}

// get returns u's usage, asking the provider if the cached one is stale.
func (c *usageCache) get(ctx context.Context, id string, u adapter.Usager) usageEntry {
	m := c.lock(id)
	m.Lock()
	defer m.Unlock()
	c.mu.Lock()
	e, ok := c.last[id]
	c.mu.Unlock()
	if ok && time.Since(e.at) < usageTTL {
		return e
	}
	usage, err := u.Usage(ctx)
	e = usageEntry{usage: usage, err: err, at: time.Now()}
	if ctx.Err() == nil {
		c.mu.Lock()
		c.last[id] = e
		c.mu.Unlock()
	}
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
