package claude

import (
	"encoding/json"
	"strings"
	"time"

	"fleet/internal/adapter"
)

// A turn stopped by a usage limit ends in a synthetic assistant message
// (model "<synthetic>"), e.g. "You've hit your session limit · resets
// 9:10pm (Europe/Vienna)", marked as an API error:
//
//	{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,
//	 "apiErrorStatus":429,"quotaLimits":{"status":"rejected",
//	 "resetsAt":1788894600,"rateLimitType":"five_hour",...},...}
//
// Subagents that hit it write the same with "isSidechain":true.

var _ adapter.Limiter = (*claude)(nil)

// limitWindows names the quotaLimits rate limit types as Usage does.
var limitWindows = map[string]string{
	"five_hour":        "5h",
	"seven_day":        "week",
	"seven_day_opus":   "week Opus",
	"seven_day_sonnet": "week Sonnet",
}

// LimitLine implements adapter.Limiter.
func (c *claude) LimitLine(line []byte, l *adapter.Limit) {
	var r struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Error       string `json:"error"`
		QuotaLimits *struct {
			ResetsAt      int64  `json:"resetsAt"`
			RateLimitType string `json:"rateLimitType"`
		} `json:"quotaLimits"`
		Message struct {
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.IsSidechain {
		return
	}
	switch r.Type {
	case "assistant":
		if r.Error == "rate_limit" {
			*l = adapter.Limit{Hit: true}
			if q := r.QuotaLimits; q != nil {
				l.Window = limitWindows[q.RateLimitType]
				if l.Window == "" {
					l.Window = strings.ReplaceAll(q.RateLimitType, "_", " ")
				}
				if q.ResetsAt > 0 {
					l.ResetsAt = time.Unix(q.ResetsAt, 0)
				}
			}
		} else if r.Message.Model != "<synthetic>" {
			*l = adapter.Limit{}
		}
	case "user":
		// A slash command (/usage, /rate-limit-options) run while waiting
		// is not the session going on.
		if !r.IsMeta && !localCommand(r.Message.Content) {
			*l = adapter.Limit{}
		}
	}
}

// localCommand reports whether a user message's content is a slash command
// or its output rather than a prompt.
func localCommand(content json.RawMessage) bool {
	var s string
	if json.Unmarshal(content, &s) != nil {
		return false
	}
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "<command-name>") || strings.HasPrefix(s, "<command-message>") ||
		strings.HasPrefix(s, "<local-command-")
}
