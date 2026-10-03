package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	c := New("", nil).(*codex)

	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"OPENAI_API_KEY":"sk-x","tokens":null}`)
	if _, err := c.Usage(context.Background()); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("api key: %v", err)
	}

	write(`{"OPENAI_API_KEY":null,"tokens":{"access_token":"tok","account_id":"acc"}}`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("ChatGPT-Account-Id") != "acc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1791612564},
			"secondary_window":{"used_percent":3.5,"limit_window_seconds":604800,"reset_at":1791612564}}}`))
	}))
	defer ts.Close()
	old := usageURL
	usageURL = ts.URL
	defer func() { usageURL = old }()

	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "pro" || len(u.Limits) != 2 || u.Limits[0].Label != "5h" || u.Limits[0].Percent != 12 ||
		u.Limits[1].Label != "week" || u.Limits[1].Percent != 3.5 || u.Limits[1].ResetsAt.Unix() != 1791612564 {
		t.Fatalf("usage %+v", u)
	}

	write(`{"tokens":{"access_token":"stale"}}`)
	if _, err := c.Usage(context.Background()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refused: %v", err)
	}
}

func TestWindowLabel(t *testing.T) {
	for secs, want := range map[int64]string{18000: "5h", 604800: "week", 86400: "1d", 259200: "3d", 1800: "30m", 0: "limit"} {
		if got := windowLabel(secs); got != want {
			t.Errorf("windowLabel(%d) = %q, want %q", secs, got, want)
		}
	}
}
