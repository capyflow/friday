// Package tools 实现机器人面向 agent 的工具集：任务调度（单次/多次/循环）、
// 当前时间、语音播报、屏幕显示、声音播放。每个工具实现 vortexagent 的
// agent.Tool 接口（Name/Description/Schema/Call）。
package tools

import (
	"bytes"
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
		"（如“每天早上8点看看我的备忘录，把今天要做的事告诉我”），适合需要查询/判断/动手的任务。" +
		"goal=true（须同时 agent=true）时升级为目标：到点后在独立会话里多轮自主推进，" +
		"直到达成、受阻或用完步数/token 预算，适合“把这件事办成”这类只给结果诉求的任务。"
}

func (t *ScheduleTaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":      map[string]any{"type": "string", "description": "任务内容或智能任务的指令，如：该敷面膜了"},
			"in":        map[string]any{"type": "string", "description": "相对时长，如 10分钟 / 2小时 / 90s / 1天。单次任务用 in 或 when 之一"},
			"when":      map[string]any{"type": "string", "description": "绝对触发时间（RFC3339，含时区），如 2026-09-01T20:00:00+08:00"},
			"every":     map[string]any{"type": "string", "description": "循环规则：自然语言“每天早上8点”“每周一三五9:30”，或 cron 表达式“0 8 * * *”"},
			// delayMinutes / scheduleRule 是管理后台表单用的形态（界面把语义算成确定值再提交）；
			// 你按自然语言安排时继续用 in / when / every。
			"delayMinutes": map[string]any{"type": "integer", "description": "多少分钟后触发一次（1-525600），与 in/when/every/scheduleRule 互斥；管理后台表单用，你优先用 in"},
			"scheduleRule": map[string]any{
				"type":        "object",
				"description": "结构化循环规则（管理后台表单用，你优先用 every）：{unit: minute|hourly|daily|weekly|monthly|yearly, interval, hour, minute, weekdays[0-6], monthDays[1-31], months[1-12]}",
				"properties": map[string]any{
					"unit":      map[string]any{"type": "string", "enum": []string{"minute", "hourly", "daily", "weekly", "monthly", "yearly"}},
					"interval":  map[string]any{"type": "integer", "description": "每 N 个单位；仅 minute/hourly 支持 N>1"},
					"hour":      map[string]any{"type": "integer", "minimum": 0, "maximum": 23},
					"minute":    map[string]any{"type": "integer", "minimum": 0, "maximum": 59},
					"weekdays":  map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 6}},
					"monthDays": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 31}},
					"months":    map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 12}},
				},
				"required": []string{"unit"},
			},
			"times":     map[string]any{"type": "integer", "description": "重复次数（≥2），必须配合 in 使用，如 in=10分钟 times=3：每10分钟一次共3次；缺省为单次"},
			"channel":   map[string]any{"type": "string", "enum": []string{"", "web", "dingtalk", "feishu", "console"}, "description": "推送通道，空为全部可用通道"},
			"status":    map[string]any{"type": "string", "enum": []string{"active", "paused"}, "description": "active立即启用（默认），paused保存后暂不触发"},
			"agent":     map[string]any{"type": "boolean", "description": "true=智能任务（agent 自主决策），text 为完整指令；默认 false（到点播报 text 原文）"},
			"goal":      map[string]any{"type": "boolean", "description": "true=目标模式（须同时 agent=true）：到点后不是一次问答，而是在独立会话里多轮自主推进直到达成/受阻/用完预算，适合「把这件事办成」这类结果导向的诉求"},
			"maxSteps":  map[string]any{"type": "integer", "description": "目标最多推进几步（默认取配置 cost.budget.perGoalSteps）"},
			"maxTokens": map[string]any{"type": "integer", "description": "目标的 token 预算（默认取配置 cost.budget.perGoalTokens），超线在步边界停下"},
			"skill":     map[string]any{"type": "string", "description": "把目标钉在某个技能上执行（技能名，见「设置 → 技能」）；留空则不启用技能"},
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
	if v, _ := args["goal"].(bool); v {
		// 目标必须是智能任务：只有 agent 类型才有"到点后自主推进"的执行器接管。
		tk.Goal = true
		tk.Kind = task.KindAgent
	}
	if v, ok := args["maxSteps"]; ok && v != nil {
		n, ok := argInt(v)
		if !ok || n < 0 {
			return task.Task{}, fmt.Errorf("无效的 maxSteps %v", v)
		}
		tk.MaxSteps = n
	}
	if v, ok := args["maxTokens"]; ok && v != nil {
		n, ok := argInt(v)
		if !ok || n < 0 {
			return task.Task{}, fmt.Errorf("无效的 maxTokens %v", v)
		}
		tk.MaxTokens = int64(n)
	}
	if sk, _ := args["skill"].(string); strings.TrimSpace(sk) != "" {
		tk.Skill = strings.TrimSpace(sk)
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

	// delayMinutes 是界面用的"多少分钟后"：整数分钟、确定性换算，
	// 不要求前端猜时区，也不走自然语言解析（in 那条留给小五说"10分钟/1天"）。
	delayMinutes := 0
	if v, ok := args["delayMinutes"]; ok && v != nil {
		n, ok := argInt(v)
		if !ok || n < 1 || n > maxDelayMinutes {
			return task.Task{}, fmt.Errorf("delayMinutes 须是 1-%d 之间的整数分钟（最长一年），当前 %v", maxDelayMinutes, v)
		}
		delayMinutes = n
	}

	// scheduleRule 是界面用的结构化循环规则，编译成 cron 落盘并原样带着回填编辑。
	var rule *task.ScheduleRule
	if v, ok := args["scheduleRule"]; ok && v != nil {
		r, err := decodeScheduleRule(v)
		if err != nil {
			return task.Task{}, err
		}
		rule = r
	}

	carrier, err := triggerCarrier(in, ws, ev, delayMinutes, rule)
	if err != nil {
		return task.Task{}, err
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
	if times >= 2 && carrier != "in" {
		return task.Task{}, fmt.Errorf("times（重复次数）只能配合 in（相对时长）使用；当前给的是 %s。"+
			"要按日历循环请用 every 或 scheduleRule，要固定时刻请用 when", carrier)
	}

	switch carrier {
	case "delayMinutes":
		tk.Kind, tk.When = orKind(tk.Kind, task.KindOnce), time.Now().Add(time.Duration(delayMinutes)*time.Minute)
	case "in":
		d, err := task.ParseHumanDuration(in)
		if err != nil {
			return task.Task{}, fmt.Errorf("无法解析时长 %q：%w（要按分钟数请用 delayMinutes）", in, err)
		}
		if d <= 0 {
			return task.Task{}, fmt.Errorf("时长必须大于零")
		}
		if times >= 2 {
			tk.Kind, tk.Interval, tk.Times = orKind(tk.Kind, task.KindRepeat), d, times
		} else {
			tk.Kind, tk.When = orKind(tk.Kind, task.KindOnce), time.Now().Add(d)
		}
	case "when":
		tm, err := time.Parse(time.RFC3339Nano, ws)
		if err != nil {
			return task.Task{}, fmt.Errorf("无法解析时间 %q：when 须是含时区的 RFC3339，如 2026-10-20T08:00:00+08:00（%v）", ws, err)
		}
		tk.Kind, tk.When = orKind(tk.Kind, task.KindOnce), tm
	case "scheduleRule":
		spec, err := rule.ToCron()
		if err != nil {
			return task.Task{}, err
		}
		tk.Kind, tk.Cron, tk.ScheduleRule = orKind(tk.Kind, task.KindCron), spec, rule
	default: // every
		spec, err := ParseHumanCron(ev)
		if err != nil {
			return task.Task{}, err
		}
		tk.Kind, tk.Cron = orKind(tk.Kind, task.KindCron), spec
	}
	return tk, nil
}

// maxDelayMinutes 是一次性延迟的上限：一年（365×24×60）。
const maxDelayMinutes = 525600

// triggerCarrier 校验触发方式的互斥并返回唯一那个。
//
// 原来只报"必须且只能提供之一"，多填时看不出是哪两个在打架。这里逐条给结论：
// 先拒"一次性 × 循环"的跨类组合（语义上根本冲突），再拒同类内的重复写法。
func triggerCarrier(in, ws, ev string, delayMinutes int, rule *task.ScheduleRule) (string, error) {
	var present []string
	if delayMinutes > 0 {
		present = append(present, "delayMinutes")
	}
	if strings.TrimSpace(in) != "" {
		present = append(present, "in")
	}
	if strings.TrimSpace(ws) != "" {
		present = append(present, "when")
	}
	if rule != nil {
		present = append(present, "scheduleRule")
	}
	if strings.TrimSpace(ev) != "" {
		present = append(present, "every")
	}
	switch len(present) {
	case 0:
		return "", fmt.Errorf("缺少触发方式：in（多久之后）、when（绝对时间）、delayMinutes（N 分钟后）、" +
			"every（cron 表达式）或 scheduleRule（结构化循环规则）必须给一个")
	case 1:
		return present[0], nil
	}
	isOnce := map[string]bool{"delayMinutes": true, "in": true, "when": true}
	var onceSide, cronSide []string
	for _, c := range present {
		if isOnce[c] {
			onceSide = append(onceSide, c)
		} else {
			cronSide = append(cronSide, c)
		}
	}
	if len(onceSide) > 0 && len(cronSide) > 0 {
		return "", fmt.Errorf("触发方式冲突：%s 是「只响一次」，%s 是「循环触发」，不能同时给。"+
			"要循环就用 every 或 scheduleRule，要响一次就用 in / when / delayMinutes",
			strings.Join(onceSide, "+"), strings.Join(cronSide, "+"))
	}
	switch strings.Join(present, "+") {
	case "delayMinutes+in":
		return "", fmt.Errorf("delayMinutes（N 分钟后）和 in（多久之后）是同一件事的两种写法，保留一个即可："+
			"界面用 delayMinutes，让小五说“10分钟后”时它用 in")
	case "delayMinutes+when", "in+when":
		return "", fmt.Errorf("一次性任务的时间点只能给一个：相对延迟（delayMinutes / in）与绝对时刻（when）同时给了无法判断以哪个为准")
	case "scheduleRule+every":
		return "", fmt.Errorf("循环方式只能选一种：结构化规则 scheduleRule 或直接写 cron 表达式 every")
	}
	return "", fmt.Errorf("触发方式只能给一个，当前同时给了 %s", strings.Join(present, "、"))
}

// decodeScheduleRule 按 JSON 标签严格解码界面上报的循环规则：
// 字段名写错要当场报错，不能被静默忽略成"每天 0 点"。
func decodeScheduleRule(v any) (*task.ScheduleRule, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("scheduleRule 格式不对：%w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r task.ScheduleRule
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("scheduleRule 格式不对：%w", err)
	}
	return &r, nil
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
