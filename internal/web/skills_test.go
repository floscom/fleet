package web

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fleet/internal/discovery"
	"fleet/internal/skills"
)

func TestSkillsSyncBetweenDaemons(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	LoadOrCreateToken(keyPath)

	var mu sync.Mutex
	found := map[string][]discovery.Found{}
	start := func(id string) (*httptest.Server, *skills.Store) {
		root := filepath.Join(dir, id)
		st, err := skills.Open(skills.Options{
			Dir: filepath.Join(root, "skills"), State: filepath.Join(root, "skills.json"), ID: id + "-id", Name: id,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		src := newFakeSource()
		src.id = id + "-id"
		ts := startServerOpts(t, Options{Source: src, KeyPath: keyPath, Skills: st,
			Browse: func(context.Context, time.Duration) ([]discovery.Found, error) {
				mu.Lock()
				defer mu.Unlock()
				return found[id], nil
			}})
		return ts, st
	}
	a, as := start("a")
	b, bs := start("b")
	ah, ap := hostPort(t, a)
	bh, bp := hostPort(t, b)
	mu.Lock()
	found["a"] = []discovery.Found{{ServerID: "b-id", Name: "b", Addrs: []string{bh}, WebPort: bp}}
	found["b"] = []discovery.Found{{ServerID: "a-id", Name: "a", Addrs: []string{ah}, WebPort: ap}}
	mu.Unlock()
	waitPeers(t, a)
	waitPeers(t, b)

	settle := func(st *skills.Store) {
		st.Scan(context.Background())
		st.Scan(context.Background())
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Made on a: b pulls it once a says its skills changed.
	skill := filepath.Join(as.Dir(), "deploy")
	os.MkdirAll(filepath.Join(skill, "scripts"), 0o755)
	os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("# deploy"), 0o644)
	os.WriteFile(filepath.Join(skill, "scripts", "run.sh"), []byte("#!/bin/sh"), 0o755)
	settle(as)
	onB := filepath.Join(bs.Dir(), "deploy", "scripts", "run.sh")
	wait("the skill on b", func() bool { _, err := os.Stat(onB); return err == nil })

	// Deleted on b: gone from a.
	os.RemoveAll(filepath.Join(bs.Dir(), "deploy"))
	settle(bs)
	wait("the skill gone from a", func() bool { _, err := os.Stat(skill); return os.IsNotExist(err) })

	// Unsigned requests get nothing.
	var e struct{ Error string }
	if code := api(t, a, "GET", "/api/skills", "", "", &e); code != 401 {
		t.Fatalf("unsigned manifest: %d", code)
	}
}
