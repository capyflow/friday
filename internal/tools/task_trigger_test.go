package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

// TestParseTaskArgsDelayMinutes 界面提交的"N 分钟后"：换算成绝对 When，Kind 为单次。
func TestParseTaskArgsDelayMinutes(t *testing.T) {
	before := time.Now()
	tk, err := ParseTaskArgs(map[string]any{"text": "该敷面膜了", "delayMinutes": float64(10)})
	if err != nil {
		t.Fatalf("ParseTaskArgs: %v", err)
	}
	if tk.Kind != task.KindOnce {
		t.Errorf("Kind 应为 once，实际 %s", tk.Kind)
	}
	want := before.Add(10 * time.Minute)
	if d := tk.When.Sub(want); d < -time.Minute || d > time.Minute {
		t.Errorf("When 应约为 10 分钟后，偏差 %v", d)
	}
	for _, bad := range []any{float64(0), float64(maxDelayMinutes + 1), "ten", -5} {
		if _, err := ParseTaskArgs(map[string]any{"text": "x", "delayMinutes": bad}); err == nil ||
			!strings.Contains(err.Error(), "delayMinutes") {
			t.Errorf("delayMinutes=%v 应被拒并点名该字段，实际 %v", bad, err)
		}
	}
}

// TestParseTaskArgsScheduleRule 结构化循环规则：编译成 cron 落盘，同时保留规则本身供回填。
func TestParseTaskArgsScheduleRule(t *testing.T) {
	cases := []struct {
		name     string
		rule     map[string]any
		wantCron string
	}{
		{"每天8:30", map[string]any{"unit": "daily", "hour": float64(8), "minute": float64(30)}, "30 8 * * *"},
		{"每周一三五9点", map[string]any{"unit": "weekly", "hour": float64(9), "weekdays": []any{float64(1), float64(3), float64(5)}}, "0 9 * * 1,3,5"},
		{"每20分钟", map[string]any{"unit": "minute", "interval": float64(20)}, "*/20 * * * *"},
	}
	for _, tc := range cases {
		tk, err := ParseTaskArgs(map[string]any{"text": "喝水", "scheduleRule": tc.rule})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tk.Kind != task.KindCron || tk.Cron != tc.wantCron {
			t.Errorf("%s: got kind=%s cron=%q want %q", tc.name, tk.Kind, tk.Cron, tc.wantCron)
		}
		if tk.ScheduleRule == nil || tk.ScheduleRule.Unit != tc.rule["unit"] {
			t.Errorf("%s: 应保留结构化规则供界面回填: %+v", tc.name, tk.ScheduleRule)
		}
	}

	// 非法规则与拼错的字段名都要当场报错，不能静默降级成"每天 0 点"。
	if _, err := ParseTaskArgs(map[string]any{"text": "x",
		"scheduleRule": map[string]any{"unit": "weekly", "hour": float64(9)}}); err == nil ||
		!strings.Contains(err.Error(), "星期几") {
		t.Errorf("weekly 缺 weekdays 应报错，实际 %v", err)
	}
	if _, err := ParseTaskArgs(map[string]any{"text": "x",
		"scheduleRule": map[string]any{"unit": "daily", "hourse": float64(9)}}); err == nil ||
		!strings.Contains(err.Error(), "scheduleRule 格式不对") {
		t.Errorf("未知字段应报错，实际 %v", err)
	}
}

// TestParseTaskArgsTriggerConflicts 触发方式互斥：每种组合给一句能照着改的话。
func TestParseTaskArgsTriggerConflicts(t *testing.T) {
	cases := []struct {
		name  string
		args  map[string]any
		wants []string
	}{
		{"什么都不给", map[string]any{"text": "x"}, []string{"缺少触发方式", "delayMinutes", "scheduleRule"}},
		{"延迟×绝对", map[string]any{"text": "x", "delayMinutes": float64(10), "when": "2026-10-20T08:00:00+08:00"},
			[]string{"一次性任务的时间点只能给一个"}},
		{"相对×循环", map[string]any{"text": "x", "in": "10分钟", "every": "0 8 * * *"},
			[]string{"触发方式冲突"}},
		{"两种循环写法", map[string]any{"text": "x", "every": "0 8 * * *",
			"scheduleRule": map[string]any{"unit": "daily"}}, []string{"循环方式只能选一种"}},
		{"两种相对写法", map[string]any{"text": "x", "in": "10分钟", "delayMinutes": float64(10)},
			[]string{"同一件事的两种写法"}},
		{"次数配错载体", map[string]any{"text": "x", "when": "2026-10-20T08:00:00+08:00", "times": float64(3)},
			[]string{"times（重复次数）只能配合 in"}},
		{"绝对时间格式不对", map[string]any{"text": "x", "when": "2026-10-20 08:00"},
			[]string{"when 须是含时区的 RFC3339"}},
		{"时长解析失败", map[string]any{"text": "x", "in": "明天8点"},
			[]string{"无法解析时长", "delayMinutes"}},
	}
	for _, tc := range cases {
		_, err := ParseTaskArgs(tc.args)
		if err == nil {
			t.Errorf("%s: 应报错", tc.name)
			continue
		}
		for _, w := range tc.wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: 错误应含 %q，实际 %q", tc.name, w, err)
			}
		}
	}
}

// TestParseTaskArgsLegacyCarriers 小五那条路径不变：in / in+times / when / every（含中文）。
func TestParseTaskArgsLegacyCarriers(t *testing.T) {
	tk, err := ParseTaskArgs(map[string]any{"text": "x", "in": "10分钟"})
	if err != nil || tk.Kind != task.KindOnce || tk.When.IsZero() {
		t.Fatalf("in 单次: kind=%s err=%v", tk.Kind, err)
	}
	tk, err = ParseTaskArgs(map[string]any{"text": "x", "in": "10分钟", "times": float64(3)})
	if err != nil || tk.Kind != task.KindRepeat || tk.Times != 3 || tk.Interval != 10*time.Minute {
		t.Fatalf("in+times 重复: %+v err=%v", tk, err)
	}
	tk, err = ParseTaskArgs(map[string]any{"text": "x", "when": "2030-10-20T08:00:00+08:00"})
	if err != nil || tk.Kind != task.KindOnce || tk.When.Year() != 2030 {
		t.Fatalf("when: %+v err=%v", tk, err)
	}
	tk, err = ParseTaskArgs(map[string]any{"text": "x", "every": "每天早上8点"})
	if err != nil || tk.Kind != task.KindCron || tk.Cron != "0 8 * * *" {
		t.Fatalf("every 中文: cron=%q err=%v", tk.Cron, err)
	}
	if tk.ScheduleRule != nil {
		t.Errorf("手写表达式不该带结构化规则: %+v", tk.ScheduleRule)
	}
}
