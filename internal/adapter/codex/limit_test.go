package codex

import (
	"testing"
	"time"

	"fleet/internal/adapter"
)

func tokenCount(primary, secondary string, reached string) string {
	return `{"timestamp":"2026-10-04T10:00:00Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":` +
		primary + `,"secondary":` + secondary + `,"credits":null,"plan_type":"plus","rate_limit_reached_type":` + reached + `}}}`
}

func TestLimitLine(t *testing.T) {
	c := &codex{}
	week := `{"used_percent":40.0,"window_minutes":10080,"resets_at":1791122039}`
	full5h := `{"used_percent":100.0,"window_minutes":300,"resets_at":1790000000}`
	fullWeek := `{"used_percent":100.0,"window_minutes":10080,"resets_at":1791122039}`
	steps := []struct {
		name, line string
		want       adapter.Limit
	}{
		{"usage", tokenCount(`{"used_percent":20.0,"window_minutes":300,"resets_at":1790000000}`, week, "null"), adapter.Limit{}},
		// The request that used the last of it may still have worked.
		{"used up", tokenCount(full5h, week, "null"), adapter.Limit{Window: "5h", ResetsAt: time.Unix(1790000000, 0)}},
		{"refused", `{"timestamp":"x","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), or try again at 9:10 PM.","codex_error_info":"usage_limit_exceeded"}}`,
			adapter.Limit{Hit: true, Window: "5h", ResetsAt: time.Unix(1790000000, 0)}},
		{"turn end", `{"timestamp":"x","type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}`,
			adapter.Limit{Hit: true, Window: "5h", ResetsAt: time.Unix(1790000000, 0)}},
		{"both used up", tokenCount(full5h, fullWeek, "null"), adapter.Limit{Hit: true, Window: "week", ResetsAt: time.Unix(1791122039, 0)}},
		{"next turn", `{"timestamp":"x","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}`, adapter.Limit{}},
		{"reached type", tokenCount(full5h, "null", `"rate_limit_reached"`), adapter.Limit{Hit: true, Window: "5h", ResetsAt: time.Unix(1790000000, 0)}},
		{"prompt", `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"continue"}]}}}`, adapter.Limit{}},
		{"message only", `{"timestamp":"x","type":"event_msg","payload":{"type":"error","message":"Usage limit reached. You've reached your usage limit. Increase your limits to continue using codex."}}`, adapter.Limit{Hit: true}},
		{"other error", `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed","item":{}}}`, adapter.Limit{}},
		{"server error", `{"timestamp":"x","type":"event_msg","payload":{"type":"error","message":"Codex is currently experiencing high load","codex_error_info":"server_overloaded"}}`, adapter.Limit{}},
		{"response item", `{"timestamp":"x","type":"response_item","payload":{"type":"message"}}`, adapter.Limit{}},
	}
	var l adapter.Limit
	for _, s := range steps {
		c.LimitLine([]byte(s.line), &l)
		if l.Hit != s.want.Hit || l.Window != s.want.Window || !l.ResetsAt.Equal(s.want.ResetsAt) {
			t.Fatalf("after %s: %+v, want %+v", s.name, l, s.want)
		}
	}
}

func TestLimitsOnScreen(t *testing.T) {
	c := &codex{}
	screen := "› fix the tests\n\n■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), or try\nagain at 9:10 PM.\n\n› continue\n\n■ You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits\n"
	if n := c.LimitsOnScreen(screen); n != 2 {
		t.Fatalf("LimitsOnScreen = %d, want 2", n)
	}
	if n := c.LimitsOnScreen("› hi\n• Hello!"); n != 0 {
		t.Fatalf("LimitsOnScreen = %d, want 0", n)
	}
}
