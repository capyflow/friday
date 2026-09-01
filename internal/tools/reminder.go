// Package tools 实现机器人面向 agent 的工具集：定时提醒、当前时间、语音播报、屏幕显示。
// 每个工具实现 vortexagent 的 agent.Tool 接口（Name/Description/Schema/Call）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/capyflow/friday/internal/reminder"
)

// ScheduleReminderTool 让 agent 安排定时提醒。
// 用户说"10分钟后提示我该敷面膜了"，agent 解析出相对时长并调用本工具。
type ScheduleReminderTool struct {
	Engine *reminder.Engine
}

func (t *ScheduleReminderTool) Name() string { return "schedule_reminder" }

func (t *ScheduleReminderTool) Description() string {
	return "安排一个定时提醒：在指定时间后通过语音和屏幕提示用户。用户说类似“10分钟后提醒我敷面膜”时使用。"
}

func (t *ScheduleReminderTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "提醒内容，如：该敷面膜了"},
			"in":   map[string]any{"type": "string", "description": "相对时长，如 10分钟 / 2小时 / 90s / 1h30m / 1天"},
			"when": map[string]any{"type": "string", "description": "可选，绝对触发时间（RFC3339），如 2026-09-01T20:00:00+08:00"},
		},
		"required": []string{"text"},
	}
}

func (t *ScheduleReminderTool) Call(_ context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("缺少参数 text")
	}

	var when time.Time
	if in, ok := args["in"].(string); ok && in != "" {
		d, err := reminder.ParseHumanDuration(in)
		if err != nil {
			return "", fmt.Errorf("无法解析时长 %q：%w", in, err)
		}
		when = time.Now().Add(d)
	} else if ws, ok := args["when"].(string); ok && ws != "" {
		tm, err := time.Parse(time.RFC3339, ws)
		if err != nil {
			return "", fmt.Errorf("无法解析时间 %q：%w", ws, err)
		}
		when = tm
	} else {
		return "", fmt.Errorf("必须提供 in（相对时长）或 when（绝对时间）之一")
	}

	id, err := t.Engine.Schedule("", when, text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已安排提醒 #%s：%q，将于 %s 触发", id, text, when.Format("2006-01-02 15:04:05")), nil
}

// ListRemindersTool 列出所有未触发的提醒。
type ListRemindersTool struct {
	Engine *reminder.Engine
}

func (t *ListRemindersTool) Name() string { return "list_reminders" }
func (t *ListRemindersTool) Description() string {
	return "列出当前所有已安排的定时提醒及其 ID 和触发时间。"
}
func (t *ListRemindersTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *ListRemindersTool) Call(_ context.Context, _ map[string]any) (string, error) {
	rs := t.Engine.List()
	if len(rs) == 0 {
		return "当前没有待触发的提醒。", nil
	}
	out := "当前提醒：\n"
	for _, r := range rs {
		out += fmt.Sprintf("- #%s 于 %s：%s\n", r.ID, r.When.Format("2006-01-02 15:04:05"), r.Text)
	}
	return out, nil
}

// CancelReminderTool 按 ID 取消一条提醒。
type CancelReminderTool struct {
	Engine *reminder.Engine
}

func (t *CancelReminderTool) Name() string { return "cancel_reminder" }
func (t *CancelReminderTool) Description() string {
	return "取消一条已安排的定时提醒。用户说“取消提醒”时，先调用 list_reminders 拿到 ID 再取消。"
}
func (t *CancelReminderTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "要取消的提醒 ID（来自 list_reminders）"},
		},
		"required": []string{"id"},
	}
}
func (t *CancelReminderTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	if t.Engine.Cancel(id) {
		return fmt.Sprintf("已取消提醒 #%s", id), nil
	}
	return fmt.Sprintf("未找到提醒 #%s", id), nil
}

// GetTimeTool 返回当前时间，供 agent 计算相对时间用。
type GetTimeTool struct{}

func (t *GetTimeTool) Name() string        { return "get_time" }
func (t *GetTimeTool) Description() string { return "获取当前时间。" }
func (t *GetTimeTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *GetTimeTool) Call(_ context.Context, _ map[string]any) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}

// marshalJSON 把 v 序列化为 JSON 字符串（工具结果用）。
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
}
