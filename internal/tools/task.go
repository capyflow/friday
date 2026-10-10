// Package tools 实现机器人面向 agent 的工具集：任务调度（单次/多次/循环）、
// 当前时间、语音播报、屏幕显示、声音播放。每个工具实现 vortexagent 的
// agent.Tool 接口（Name/Description/Schema/Call）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/task"
)

// ScheduleTaskTool 让 agent 安排任务：单次、有限多次或无限循环。
type ScheduleTaskTool struct {
	Engine *task.Engine
}

func (t *ScheduleTaskTool) Name() string { return "schedule_task" }

func (t *ScheduleTaskTool) Description() string {
	return "安排定时任务，三种类型三选一：" +
		"1) 单次——用户说“10分钟后提醒我敷面膜”填 in；说“明天早上8点叫我起床”先 get_time 再填 when；" +
		"2) 有限多次——用户说“每10分钟提醒我一次，共3次”填 in 加 times；" +
		"3) 无限循环——用户说“每天早上8点提醒我喝水”“每周一三五9点播报天气”填 every。" +
		"agent=true 时为智能任务：到点把 text 作为指令交给 agent 自主决策与行动" +
		"（如“每天早上8点看看我的备忘录，把今天要做的事告诉我”），适合需要查询/判断/动手的任务。"
}

func (t *ScheduleTaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":    map[string]any{"type": "string", "description": "任务内容或智能任务的指令，如：该敷面膜了"},
			"in":      map[string]any{"type": "string", "description": "相对时长，如 10分钟 / 2小时 / 90s / 1天。单次任务用 in 或 when 之一"},
			"when":    map[string]any{"type": "string", "description": "绝对触发时间（RFC3339，含时区），如 2026-09-01T20:00:00+08:00"},
			"every":   map[string]any{"type": "string", "description": "循环规则：自然语言“每天早上8点”“每周一三五9:30”，或 cron 表达式“0 8 * * *”"},
			"times":   map[string]any{"type": "integer", "description": "重复次数（≥2），必须配合 in 使用，如 in=10分钟 times=3：每10分钟一次共3次；缺省为单次"},
			"channel": map[string]any{"type": "string", "enum": []string{"", "web", "dingtalk", "feishu", "console"}, "description": "推送通道，空为全部可用通道"},
			"status":  map[string]any{"type": "string", "enum": []string{"active", "paused"}, "description": "active立即启用（默认），paused保存后暂不触发"},
			"agent":   map[string]any{"type": "boolean", "description": "true=智能任务（agent 自主决策），text 为完整指令；默认 false（到点播报 text 原文）"},
		},
		"required": []string{"text"},
	}
}

// ParseTaskArgs 共用 HTTP 与工具的完整任务定义解析，不接受客户端关联待办字段。
func ParseTaskArgs(args map[string]any) (task.Task, error) {
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return task.Task{}, fmt.Errorf("缺少参数 text")
	}
	tk := task.Task{Text: text}
	if v, _ := args["agent"].(bool); v {
		tk.Kind = task.KindAgent
	}
	if channel, _ := args["channel"].(string); channel != "" {
		switch channel {
		case "web", "dingtalk", "feishu", "console":
			tk.Channel = channel
		default:
			return task.Task{}, fmt.Errorf("无效的通知通道 %q", channel)
		}
	}
	if status, _ := args["status"].(string); status != "" {
		if status != task.StatusActive && status != task.StatusPaused {
			return task.Task{}, fmt.Errorf("无效的任务状态 %q", status)
		}
		tk.Status = status
	}
	ev, _ := args["every"].(string)
	in, _ := args["in"].(string)
	ws, _ := args["when"].(string)
	count := 0
	for _, value := range []string{ev, in, ws} {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	if count != 1 {
		return task.Task{}, fmt.Errorf("必须且只能提供 in（相对时长）、when（绝对时间）或 every（循环规则）之一")
	}
	times := 1
	if v, ok := args["times"]; ok && v != nil {
		n, ok := argInt(v)
		if !ok {
			return task.Task{}, fmt.Errorf("无效的 times %v", v)
		}
		times = n
	}
	if times < 1 {
		return task.Task{}, fmt.Errorf("times 至少为 1")
	}
	if times >= 2 && strings.TrimSpace(in) == "" {
		return task.Task{}, fmt.Errorf("times 必须配合 in（相对时长）使用")
	}
	if ev != "" {
		spec, err := ParseHumanCron(ev)
		if err != nil {
			return task.Task{}, err
		}
		tk.Kind, tk.Cron = orKind(tk.Kind, task.KindCron), spec
	} else if in != "" {
		d, err := task.ParseHumanDuration(in)
		if err != nil {
			return task.Task{}, fmt.Errorf("无法解析时长 %q：%w", in, err)
		}
		if d <= 0 {
			return task.Task{}, fmt.Errorf("时长必须大于零")
		}
		if times >= 2 {
			tk.Kind, tk.Interval, tk.Times = orKind(tk.Kind, task.KindRepeat), d, times
		} else {
			tk.Kind, tk.When = orKind(tk.Kind, task.KindOnce), time.Now().Add(d)
		}
	} else {
		tm, err := time.Parse(time.RFC3339Nano, ws)
		if err != nil {
			return task.Task{}, fmt.Errorf("无法解析时间 %q：%w", ws, err)
		}
		tk.Kind, tk.When = orKind(tk.Kind, task.KindOnce), tm
	}
	return tk, nil
}

func (t *ScheduleTaskTool) Call(_ context.Context, args map[string]any) (string, error) {
	tk, err := ParseTaskArgs(args)
	if err != nil {
		return "", err
	}
	tk, err = t.Engine.Schedule(tk)
	if err != nil {
		return "", err
	}
	label := map[task.Kind]string{task.KindOnce: "单次任务", task.KindRepeat: "多次任务", task.KindCron: "循环任务", task.KindAgent: "智能任务"}[tk.Kind]
	if tk.Status == task.StatusPaused {
		return fmt.Sprintf("已保存暂停的%s #%s：%q，原计划 %s；恢复前不会触发", label, tk.ID, tk.Text, tk.Next.Format("2006-01-02 15:04:05")), nil
	}
	return fmt.Sprintf("已安排%s #%s：%q，下次触发 %s", label, tk.ID, tk.Text, tk.Next.Format("2006-01-02 15:04:05")), nil
}

// UpdateTaskTool 完整替换未来任务定义，已经入队的执行保持原记录。
type UpdateTaskTool struct{ Engine *task.Engine }

func (t *UpdateTaskTool) Name() string { return "update_task" }
func (t *UpdateTaskTool) Description() string {
	return "编辑现有任务的完整定义：id、text 与 in/when/every 之一必填，可设 times、agent、channel；保留暂停状态，仅影响未来安排，已经入队的执行不撤销。先 list_tasks 获取 ID。"
}
func (t *UpdateTaskTool) Schema() map[string]any {
	schema := (&ScheduleTaskTool{}).Schema()
	schema["properties"].(map[string]any)["id"] = map[string]any{"type": "string", "description": "现有任务 ID"}
	delete(schema["properties"].(map[string]any), "status")
	schema["required"] = []string{"id", "text"}
	return schema
}
func (t *UpdateTaskTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	tk, err := ParseTaskArgs(args)
	if err != nil {
		return "", err
	}
	tk, err = t.Engine.Update(id, tk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已更新任务 #%s，原计划下次 %s；仅影响未来安排，已入队执行保持原记录", tk.ID, tk.Next.Format("2006-01-02 15:04:05")), nil
}

type PauseTaskTool struct{ Engine *task.Engine }

func (t *PauseTaskTool) Name() string { return "pause_task" }
func (t *PauseTaskTool) Description() string {
	return "暂停任务的未来触发，保留任务定义与原计划，不撤销已经入队的执行。先 list_tasks 获取 ID。"
}
func (t *PauseTaskTool) Schema() map[string]any { return taskIDSchema() }
func (t *PauseTaskTool) Call(_ context.Context, args map[string]any) (string, error) {
	return changePause(t.Engine, args, true)
}

type ResumeTaskTool struct{ Engine *task.Engine }

func (t *ResumeTaskTool) Name() string { return "resume_task" }
func (t *ResumeTaskTool) Description() string {
	return "恢复暂停任务：过去的单次立即补触发，周期任务跳至下一未来时刻，不补跑暂停期间周期且不消费次数。"
}
func (t *ResumeTaskTool) Schema() map[string]any { return taskIDSchema() }
func (t *ResumeTaskTool) Call(_ context.Context, args map[string]any) (string, error) {
	return changePause(t.Engine, args, false)
}
func taskIDSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "description": "任务 ID"}}, "required": []string{"id"}}
}
func changePause(engine *task.Engine, args map[string]any, paused bool) (string, error) {
	id, _ := args["id"].(string)
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	var tk task.Task
	var err error
	if paused {
		tk, err = engine.Pause(id)
	} else {
		tk, err = engine.Resume(id)
	}
	if err != nil {
		return "", err
	}
	if paused {
		return fmt.Sprintf("已暂停任务 #%s 的未来触发；已入队的执行不受影响", tk.ID), nil
	}
	return fmt.Sprintf("已恢复任务 #%s，下次 %s", tk.ID, tk.Next.Format("2006-01-02 15:04:05")), nil
}

// orKind 返回智能任务类型（agent）或默认类型：agent 任务沿用各分支的时间语义。
func orKind(kind, def task.Kind) task.Kind {
	if kind != "" {
		return kind
	}
	return def
}

// ListTasksTool 列出所有待触发任务（单次/多次/循环）。
type ListTasksTool struct {
	Engine *task.Engine
}

func (t *ListTasksTool) Name() string { return "list_tasks" }
func (t *ListTasksTool) Description() string {
	return "列出当前所有待触发任务（含单次、多次、循环）及其 ID、类型和下次触发时间。"
}
func (t *ListTasksTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *ListTasksTool) Call(_ context.Context, _ map[string]any) (string, error) {
	if err := t.Engine.Err(); err != nil {
		return "", err
	}
	tasks := t.Engine.List()
	if len(tasks) == 0 {
		return "当前没有待触发的任务。", nil
	}
	out := "当前任务：\n"
	for _, tk := range tasks {
		status := "启用"
		if tk.Status == task.StatusPaused {
			status = "已暂停，原计划"
		}
		out += fmt.Sprintf("- #%s [%s/%s] %s %s（剩余%d次，cron=%s，通道=%s）\n", tk.ID, tk.Kind, status, tk.Text, tk.Next.Format("2006-01-02 15:04:05"), tk.Remaining, tk.Cron, tk.Channel)
	}
	return out, nil
}

// CancelTaskTool 按 ID 取消一条任务（单次/多次/循环通用）。
type CancelTaskTool struct {
	Engine *task.Engine
}

func (t *CancelTaskTool) Name() string { return "cancel_task" }
func (t *CancelTaskTool) Description() string {
	return "取消一条已安排的任务（单次/多次/循环通用）。先调用 list_tasks 拿到 ID 再取消。"
}
func (t *CancelTaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "要取消的任务 ID（来自 list_tasks）"},
		},
		"required": []string{"id"},
	}
}
func (t *CancelTaskTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	ok, err := t.Engine.CancelErr(id)
	if err != nil {
		return "", err
	}
	if ok {
		return fmt.Sprintf("已取消任务 #%s", id), nil
	}
	return fmt.Sprintf("未找到任务 #%s", id), nil
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

// argInt 把工具参数中的数字（JSON 数字或字符串）转为 int。
func argInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n >= float64(math.MaxInt) || n < float64(math.MinInt) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// marshalJSON 把 v 序列化为 JSON 字符串（工具结果用）。
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
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
	return task.ParseCron(trimmed)
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
