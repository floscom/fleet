package claude

import (
	"testing"
	"time"

	"fleet/internal/adapter"
)

// limitLine is what Claude Code 2.1.261 wrote when the 5-hour limit
// stopped a turn (shortened).
const limitLine = `{"parentUuid":"d38c","isSidechain":false,"type":"assistant","uuid":"d010","timestamp":"2026-09-08T15:38:49.013Z","message":{"id":"67e7","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"You've hit your session limit · resets 9:10pm (Europe/Vienna)"}]},"requestId":"req_011","quotaLimits":{"status":"rejected","resetsAt":1788894600,"unifiedRateLimitFallbackAvailable":false,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false},"error":"rate_limit","isApiErrorMessage":true,"apiErrorStatus":429,"sessionId":"7572","version":"2.1.261"}`

func TestLimitLine(t *testing.T) {
	c := &claude{}
	hit := adapter.Limit{Hit: true, Window: "5h", ResetsAt: time.Unix(1788894600, 0)}
	steps := []struct {
		name, line string
		want       adapter.Limit
	}{
		{"prompt", `{"type":"user","message":{"role":"user","content":"go on"}}`, adapter.Limit{}},
		{"limit", limitLine, hit},
		{"subagent", `{"type":"user","isSidechain":true,"message":{"role":"user","content":"x"}}`, hit},
		{"meta", `{"type":"user","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image]"}]}}`, hit},
		{"command", `{"type":"user","message":{"role":"user","content":"<command-name>/usage</command-name>"}}`, hit},
		{"command output", `{"type":"user","message":{"role":"user","content":"<local-command-stdout>5h: 100%</local-command-stdout>"}}`, hit},
		{"system", `{"type":"system","subtype":"turn_duration"}`, hit},
		{"synthetic", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`, hit},
		{"resumed", `{"type":"user","message":{"role":"user","content":"continue"}}`, adapter.Limit{}},
		{"limit again", limitLine, hit},
		{"reply", `{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"On it."}]}}`, adapter.Limit{}},
		{"no quota", `{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,"message":{"model":"<synthetic>"}}`, adapter.Limit{Hit: true}},
		{"broken", `{"type":`, adapter.Limit{Hit: true}},
	}
	var l adapter.Limit
	for _, s := range steps {
		c.LimitLine([]byte(s.line), &l)
		if l.Hit != s.want.Hit || l.Window != s.want.Window || !l.ResetsAt.Equal(s.want.ResetsAt) {
			t.Fatalf("after %s: %+v, want %+v", s.name, l, s.want)
		}
	}
}
