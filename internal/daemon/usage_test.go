package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fleet/internal/adapter"
)

type fakeUsager struct {
	calls int
	usage *adapter.Usage
	err   error
}

func (f *fakeUsager) Usage(context.Context) (*adapter.Usage, error) {
	f.calls++
	return f.usage, f.err
}

func TestUsageCacheKeepsLastUsage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.json")
	week := adapter.UsageLimit{Label: "week", Percent: 40, ResetsAt: time.Now().Add(time.Hour)}
	five := adapter.UsageLimit{Label: "5h", Percent: 100, ResetsAt: time.Now().Add(-time.Minute)}
	f := &fakeUsager{usage: &adapter.Usage{Plan: "max", Limits: []adapter.UsageLimit{week, five}}}

	c := &usageCache{path: path}
	e := c.get(ctx, "claude", f)
	if e.err != nil || e.usage == nil || f.calls != 1 {
		t.Fatalf("first get: %+v, %d calls", e, f.calls)
	}
	// A window that has reset shows unused.
	if l := e.usage.Limits[1]; l.Percent != 0 || !l.ResetsAt.IsZero() {
		t.Fatalf("reset window: %+v", l)
	}
	if c.get(ctx, "claude", f); f.calls != 1 {
		t.Fatalf("fresh usage asked again")
	}

	// Stale: the provider refuses; the last usage stays, with the error,
	// and it is not asked again right away.
	c.last["claude"] = usageEntry{usage: c.last["claude"].usage, at: time.Now().Add(-usageTTL)}
	f.err = errors.New("429")
	e = c.get(ctx, "claude", f)
	if e.err == nil || e.usage == nil || e.usage.Plan != "max" || f.calls != 2 {
		t.Fatalf("failed renewal: %+v, %d calls", e, f.calls)
	}
	if c.get(ctx, "claude", f); f.calls != 2 {
		t.Fatalf("asked again before the retry delay")
	}

	// A restarted daemon starts from the file.
	c2 := &usageCache{path: path}
	f2 := &fakeUsager{err: errors.New("429")}
	e = c2.get(ctx, "claude", f2)
	if e.usage == nil || e.usage.Limits[0].Percent != 40 {
		t.Fatalf("after restart: %+v", e)
	}
}
