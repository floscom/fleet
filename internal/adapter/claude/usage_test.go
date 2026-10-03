package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	c := New("", nil).(*claude)

	if _, err := c.Usage(context.Background()); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("no login: %v", err)
	}

	exp := time.Now().Add(time.Hour).UnixMilli()
	cred := `{"claudeAiOauth":{"accessToken":"tok","expiresAt":` + itoa(exp) + `,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":18.0,"resets_at":"2026-10-03T14:00:00.29+00:00"},
			"limits":[{"kind":"session","percent":18,"resets_at":"2026-10-03T14:00:00.29+00:00"},
			{"kind":"weekly_all","percent":54,"resets_at":"2026-10-06T13:00:00+00:00"},
			{"kind":"weekly_scoped","percent":2,"scope":{"model":{"display_name":"Fable"}}}]}`))
	}))
	defer ts.Close()
	old := usageURL
	usageURL = ts.URL
	defer func() { usageURL = old }()

	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "max 20x" || len(u.Limits) != 3 {
		t.Fatalf("usage %+v", u)
	}
	want := []struct {
		label string
		pct   float64
	}{{"5h", 18}, {"week", 54}, {"week Fable", 2}}
	for i, w := range want {
		if l := u.Limits[i]; l.Label != w.label || l.Percent != w.pct {
			t.Fatalf("limit %d = %+v, want %v", i, l, w)
		}
	}
	if u.Limits[0].ResetsAt.IsZero() || !u.Limits[2].ResetsAt.IsZero() {
		t.Fatalf("resets: %+v", u.Limits)
	}

	// Older replies without "limits".
	u, err = parseUsage([]byte(`{"five_hour":{"utilization":7,"resets_at":"2026-10-03T14:00:00Z"},"seven_day":{"utilization":40},"seven_day_opus":null}`))
	if err != nil || len(u.Limits) != 2 || u.Limits[0].Label != "5h" || u.Limits[1].Percent != 40 {
		t.Fatalf("fallback %+v %v", u, err)
	}

	cred = strings.Replace(cred, itoa(exp), itoa(time.Now().Add(-time.Minute).UnixMilli()), 1)
	os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(cred), 0o600)
	if _, err := c.Usage(context.Background()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
