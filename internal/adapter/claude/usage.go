package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"fleet/internal/adapter"
)

// usageURL is what Claude Code's /usage asks: the plan limits of the
// account an OAuth access token belongs to. It is read-only and takes the
// token as it is in the credentials file, so fleet never refreshes it (a
// refresh rotates the refresh token under the CLI's feet).
var usageURL = "https://api.anthropic.com/api/oauth/usage"

// usageClient is replaced by tests.
var usageClient = &http.Client{Timeout: 8 * time.Second}

// Usage implements adapter.Usager.
func (c *claude) Usage(ctx context.Context) (*adapter.Usage, error) {
	raw, err := os.ReadFile(c.AuthFile(""))
	if errors.Is(err, fs.ErrNotExist) {
		if runtime.GOOS == "darwin" {
			return nil, errors.New("the login is in the macOS Keychain, which fleet does not read")
		}
		return nil, errors.New("not logged in")
	}
	if err != nil {
		return nil, err
	}
	var cred struct {
		OAuth *struct {
			AccessToken      string `json:"accessToken"`
			ExpiresAt        int64  `json:"expiresAt"`
			SubscriptionType string `json:"subscriptionType"`
			RateLimitTier    string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("reading the login: %w", err)
	}
	o := cred.OAuth
	if o == nil || o.AccessToken == "" {
		return nil, errors.New("not logged in with a Claude subscription")
	}
	if o.ExpiresAt > 0 && time.UnixMilli(o.ExpiresAt).Before(time.Now()) {
		return nil, errors.New("the login expired; claude renews it on its next start")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "fleet")
	resp, err := usageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("the login was refused; claude renews it on its next start")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage request failed: %s", resp.Status)
	}
	u, err := parseUsage(body)
	if err != nil {
		return nil, err
	}
	u.Plan = planName(o.SubscriptionType, o.RateLimitTier)
	return u, nil
}

// tierSize finds the multiplier in a rate limit tier such as
// "default_claude_max_20x".
var tierSize = regexp.MustCompile(`_(\d+x)$`)

func planName(sub, tier string) string {
	if m := tierSize.FindStringSubmatch(tier); m != nil && sub != "" {
		return sub + " " + m[1]
	}
	return sub
}

// usageWindow is a window in the usage reply.
type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// parseUsage reads the usage reply. Its "limits" list names every window
// (Claude Code 2.1.28x); older replies only have the fixed fields.
func parseUsage(body []byte) (*adapter.Usage, error) {
	var r struct {
		Limits []struct {
			Kind     string  `json:"kind"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
		FiveHour       *usageWindow `json:"five_hour"`
		SevenDay       *usageWindow `json:"seven_day"`
		SevenDayOpus   *usageWindow `json:"seven_day_opus"`
		SevenDaySonnet *usageWindow `json:"seven_day_sonnet"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("reading the usage reply: %w", err)
	}
	u := &adapter.Usage{}
	for _, l := range r.Limits {
		label := map[string]string{"session": "5h", "weekly_all": "week", "weekly_scoped": "week"}[l.Kind]
		if label == "" {
			label = strings.ReplaceAll(l.Kind, "_", " ")
		}
		if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
			label += " " + l.Scope.Model.DisplayName
		}
		u.Limits = append(u.Limits, adapter.UsageLimit{Label: label, Percent: l.Percent, ResetsAt: parseTime(l.ResetsAt)})
	}
	if len(u.Limits) > 0 {
		return u, nil
	}
	for _, w := range []struct {
		label string
		w     *usageWindow
	}{{"5h", r.FiveHour}, {"week", r.SevenDay}, {"week Opus", r.SevenDayOpus}, {"week Sonnet", r.SevenDaySonnet}} {
		if w.w != nil && w.w.Utilization != nil {
			u.Limits = append(u.Limits, adapter.UsageLimit{Label: w.label, Percent: *w.w.Utilization, ResetsAt: parseTime(w.w.ResetsAt)})
		}
	}
	return u, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
