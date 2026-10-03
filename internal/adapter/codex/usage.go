package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"time"

	"fleet/internal/adapter"
)

// usageURL is what Codex's /status asks: the rate limit windows of the
// ChatGPT account it is logged in with. Fleet sends the access token as it
// is in auth.json and never refreshes it.
var usageURL = "https://chatgpt.com/backend-api/wham/usage"

// usageClient is replaced by tests.
var usageClient = &http.Client{Timeout: 8 * time.Second}

// Usage implements adapter.Usager.
func (c *codex) Usage(ctx context.Context) (*adapter.Usage, error) {
	raw, err := os.ReadFile(c.AuthFile(""))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("not logged in (or the login is in the OS keyring)")
	}
	if err != nil {
		return nil, err
	}
	var auth struct {
		APIKey *string `json:"OPENAI_API_KEY"`
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, fmt.Errorf("reading the login: %w", err)
	}
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		if auth.APIKey != nil && *auth.APIKey != "" {
			return nil, errors.New("logged in with an API key, which has no plan limits")
		}
		return nil, errors.New("not logged in with ChatGPT")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	if auth.Tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", auth.Tokens.AccountID)
	}
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
		return nil, errors.New("the login was refused; codex renews it on its next start")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage request failed: %s", resp.Status)
	}
	return parseUsage(body)
}

type usageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	Seconds     int64   `json:"limit_window_seconds"`
	ResetAt     int64   `json:"reset_at"`
}

func parseUsage(body []byte) (*adapter.Usage, error) {
	var r struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *usageWindow `json:"primary_window"`
			Secondary *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("reading the usage reply: %w", err)
	}
	u := &adapter.Usage{Plan: r.PlanType}
	if r.RateLimit == nil {
		return u, nil
	}
	for _, w := range []*usageWindow{r.RateLimit.Primary, r.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		l := adapter.UsageLimit{Label: windowLabel(w.Seconds), Percent: w.UsedPercent}
		if w.ResetAt > 0 {
			l.ResetsAt = time.Unix(w.ResetAt, 0)
		}
		u.Limits = append(u.Limits, l)
	}
	return u, nil
}

// windowLabel names a window of secs seconds: "5h", "week", "3d".
func windowLabel(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d == 7*24*time.Hour:
		return "week"
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return "limit"
}
