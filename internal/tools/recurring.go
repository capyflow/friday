package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/capyflow/friday/internal/reminder"
)

// ScheduleRecurringTool 让 agent 安排循环任务（每日/每周定时主动关心用户）。
type ScheduleRecurringTool struct {
	Engine *reminder.Engine
}

func (t *ScheduleRecurringTool) Name() string { return "schedule_recurring" }

func (t *ScheduleRecurringTool) Description() string {
	return "安排循环任务：在每天/每周固定时间重复触发。用户说“每天早上8点提醒我喝水”“每周一9点播报天气”时使用。"
}

func (t *ScheduleRecurringTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "任务内容，如：提醒喝水"},
			"when": map[string]any{"type": "string", "description": "触发时间，两种格式：1) 自然语言“每天早上8点”“每周一三五9:30”；2) cron 表达式“0 8 * * *”"},
		},
		"required": []string{"text", "when"},
	}
}

func (t *ScheduleRecurringTool) Call(_ context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	when, _ := args["when"].(string)
	if text == "" || when == "" {
		return "", fmt.Errorf("缺少参数 text 或 when")
	}
	cronSpec, err := ParseHumanCron(when)
	if err != nil {
		return "", err
	}
	id, err := t.Engine.ScheduleRecurring("", cronSpec, text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已安排循环任务 #%s：%q，cron=%s", id, text, cronSpec), nil
}

// ListRecurringTool 列出所有循环任务。
type ListRecurringTool struct {
	Engine *reminder.Engine
}

func (t *ListRecurringTool) Name() string { return "list_recurring" }
func (t *ListRecurringTool) Description() string {
	return "列出当前所有循环任务及其 ID 和触发计划。"
}
func (t *ListRecurringTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *ListRecurringTool) Call(_ context.Context, _ map[string]any) (string, error) {
	rs := t.Engine.ListRecurring()
	if len(rs) == 0 {
		return "当前没有循环任务。", nil
	}
	out := "当前循环任务：\n"
	for _, r := range rs {
		out += fmt.Sprintf("- #%s cron=%s：%s\n", r.ID, r.Cron, r.Text)
	}
	return out, nil
}

// CancelRecurringTool 按 ID 取消循环任务。
type CancelRecurringTool struct {
	Engine *reminder.Engine
}

func (t *CancelRecurringTool) Name() string { return "cancel_recurring" }
func (t *CancelRecurringTool) Description() string {
	return "取消一条循环任务。先调用 list_recurring 拿到 ID 再取消。"
}
func (t *CancelRecurringTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "要取消的循环任务 ID"},
		},
		"required": []string{"id"},
	}
}
func (t *CancelRecurringTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	if t.Engine.CancelRecurring(id) {
		return fmt.Sprintf("已取消循环任务 #%s", id), nil
	}
	return fmt.Sprintf("未找到循环任务 #%s", id), nil
}

var (
	timeRe = regexp.MustCompile(`(\d{1,2})\s*[:点时]\s*(\d{1,2})?`)
	dayRe  = regexp.MustCompile(`[一二三四五六日天]`)
)

// ParseHumanCron 把自然语言时间或 cron 表达式转为 cron 表达式。
// 支持："每天早上8点" / "每天早上8:30" / "每周一三五9点" / 原生 cron "0 8 * * *"。
func ParseHumanCron(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	daily := strings.Contains(trimmed, "每天") || strings.Contains(trimmed, "每日")
	weekly := strings.Contains(trimmed, "每周") || strings.Contains(trimmed, "每星期")
	if daily || weekly {
		return parseHumanSchedule(trimmed, daily)
	}
	return reminder.ParseCron(trimmed)
}

func parseHumanSchedule(trimmed string, daily bool) (string, error) {
	weekly := strings.Contains(trimmed, "每周") || strings.Contains(trimmed, "每星期")
	t := convertCnTime(trimmed, weekly)
	m := timeRe.FindStringSubmatch(t)
	if m == nil {
		return "", fmt.Errorf("无法从 %q 解析出时刻", trimmed)
	}
	hour, _ := strconv.Atoi(m[1])
	if strings.Contains(trimmed, "晚上") || strings.Contains(trimmed, "下午") || strings.Contains(trimmed, "傍晚") {
		if hour < 12 {
			hour += 12
		}
	}
	if hour < 0 || hour > 23 {
		return "", fmt.Errorf("小时超出范围: %d", hour)
	}
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	if minute < 0 || minute > 59 {
		return "", fmt.Errorf("分钟超出范围: %d", minute)
	}

	if daily {
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	}
	days := dayRe.FindAllString(trimmed, -1)
	if len(days) == 0 {
		return "", fmt.Errorf("无法从 %q 解析出星期几", trimmed)
	}
	nums := make([]string, 0, len(days))
	for _, d := range days {
		nums = append(nums, strconv.Itoa(chineseDayToNum(d)))
	}
	return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(nums, ",")), nil
}

func chineseDayToNum(d string) int {
	// 星期日/天 → 0（cron 的 0 与 7 都是周日）。
	switch d {
	case "一":
		return 1
	case "二":
		return 2
	case "三":
		return 3
	case "四":
		return 4
	case "五":
		return 5
	case "六":
		return 6
	default:
		return 0
	}
}

var cnDigits = map[string]int{
	"零": 0, "一": 1, "二": 2, "三": 3, "四": 4,
	"五": 5, "六": 6, "七": 7, "八": 8, "九": 9,
}

func isCnDigitChar(r rune) bool {
	_, ok := cnDigits[string(r)]
	return ok || r == '十'
}

// convertCnTime 把中文数字时刻转成 ASCII 数字（"九点"→"9点"，"二十三点"→"23点"）。
func convertCnTime(s string, weekly bool) string {
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '点' || c == '时' || c == '分' {
			start := len(out) - 1
			for start >= 0 && len(out)-1-start < 3 && isCnDigitChar(out[start]) {
				start--
			}
			start++
			if n, ok := cnNumToInt(string(out[start:]), weekly); ok {
				out = out[:start]
				out = append(out, []rune(strconv.Itoa(n))...)
			}
			out = append(out, c)
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// cnNumToInt 把中文数字串转成整数；weekly 场景下"每周一九点"取最后一位为时刻。
func cnNumToInt(s string, weekly bool) (int, bool) {
	if weekly {
		r := []rune(s)
		if len(r) == 2 && !strings.ContainsRune(s, '十') {
			s = string(r[len(r)-1:])
		}
	}
	if v, ok := cnDigits[s]; ok {
		return v, true
	}
	if strings.ContainsRune(s, '十') {
		parts := strings.Split(s, "十")
		tens := 1
		if parts[0] != "" {
			tens = cnDigits[parts[0]]
		}
		ones := 0
		if len(parts) > 1 && parts[1] != "" {
			ones = cnDigits[parts[1]]
		}
		return tens*10 + ones, true
	}
	return 0, false
}
