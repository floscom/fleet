package codex

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"fleet/internal/adapter"
)

// A turn stopped by a usage limit shows up in a rollout as
//
//	token_count  rate_limits as the refusal reported them: the window
//	             that ran out at used_percent 100, with its resets_at
//	error        "You've hit your usage limit. ... try again at 9:10 PM.",
//	             codex_error_info "usage_limit_exceeded"
//
// (event_msg payload types). Whether the error is written depends on the
// version, so the screen is checked too (LimitsOnScreen). The next turn
// starts with an item_completed for its prompt.

var (
	_ adapter.Limiter       = (*codex)(nil)
	_ adapter.ScreenLimiter = (*codex)(nil)
)

// rateWindow is a window of a token_count's rate_limits.
type rateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// LimitLine implements adapter.Limiter. A window used up is remembered
// (Window, ResetsAt) without Hit: a turn may end well on the request that
// used the last of it; only a refusal sets Hit.
func (c *codex) LimitLine(line []byte, l *adapter.Limit) {
	var rl rolloutLine
	if !decode(line, &rl) || rl.Type != "event_msg" {
		return
	}
	var ev struct {
		Type       string `json:"type"`
		Message    string `json:"message"`
		RateLimits *struct {
			Primary     *rateWindow     `json:"primary"`
			Secondary   *rateWindow     `json:"secondary"`
			ReachedType json.RawMessage `json:"rate_limit_reached_type"`
		} `json:"rate_limits"`
		ErrorInfo json.RawMessage `json:"codex_error_info"`
	}
	if !decode(rl.Payload, &ev) {
		return
	}
	switch ev.Type {
	case "token_count":
		r := ev.RateLimits
		if r == nil {
			return
		}
		var out *rateWindow
		for _, w := range []*rateWindow{r.Primary, r.Secondary} {
			// The longest wait counts: both must have reset.
			if w != nil && w.UsedPercent >= 100 && (out == nil || w.ResetsAt > out.ResetsAt) {
				out = w
			}
		}
		switch {
		case out != nil:
			l.Window = windowLabel(out.WindowMinutes * 60)
			l.ResetsAt = time.Time{}
			if out.ResetsAt > 0 {
				l.ResetsAt = time.Unix(out.ResetsAt, 0)
			}
		case !l.Hit:
			l.Window, l.ResetsAt = "", time.Time{}
		}
		if reached := bytes.TrimSpace(r.ReachedType); len(reached) > 0 && !bytes.Equal(reached, []byte("null")) {
			l.Hit = true
		}
	case "error":
		if bytes.Contains(ev.ErrorInfo, []byte("usage_limit_exceeded")) || limitMessage(ev.Message) {
			l.Hit = true
		}
	case "item_completed", "task_started":
		if l.Hit {
			*l = adapter.Limit{}
		}
	}
}

// LimitsOnScreen implements adapter.ScreenLimiter.
func (c *codex) LimitsOnScreen(screen string) int {
	n := 0
	for _, l := range strings.Split(screen, "\n") {
		if limitMessage(l) {
			n++
		}
	}
	return n
}

// limitMessage reports whether s has Codex's message for a turn refused
// by a usage limit.
func limitMessage(s string) bool {
	return strings.Contains(s, "You've hit your usage limit") || strings.Contains(s, "Usage limit reached")
}
