// heartbeat.go 实现智能任务运行器（M3 心跳）：task.KindAgent 到点后不再是
// "复读预设文案"，而是把指令（心跳任务附带 HEARTBEAT.md 检查清单）交给 agent
// 自主决策与行动。输出协议：回答首行 [推送] / [静默]——[推送] 把正文投递到
// 可用通道（本地播报/钉钉/飞书），[静默] 只记轨迹不打扰；静默时段（quietHours）
// 内心跳照常执行但一律不推送。
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/trace"
)

// Notifier 是主动消息（心跳/智能任务输出）的投递目标。
type Notifier interface {
	// Name 返回通道名（与 task.Task.Channel 匹配；空 Channel 投递全部）。
	Name() string
	// Notify 投递一条主动消息。
	Notify(ctx context.Context, text string) error
}

// ConsoleNotifier 本地输出：播报 + 屏幕显示（硬件输出开启时生效，当前为 console 实现）。
type ConsoleNotifier struct {
	Speaker   output.Speaker
	Displayer output.Displayer
}

func (n *ConsoleNotifier) Name() string { return "console" }

func (n *ConsoleNotifier) Notify(ctx context.Context, text string) error {
	if n.Speaker == nil && n.Displayer == nil {
		return fmt.Errorf("未配置本地输出")
	}
	if n.Speaker != nil {
		if err := n.Speaker.Speak(ctx, text); err != nil {
			return err
		}
	}
	if n.Displayer != nil {
		return n.Displayer.Display(ctx, text)
	}
	return nil
}

// dialogue 抽象对话能力（*Brain 实现之；测试可注入假实现）。
// 用 AskWith 而非 Ask：智能任务的提示词是机器生成的长文本，必须显式关闭
// 技能自动匹配（否则拿"心跳检查清单"去猜用户技能，结果不可预测）。
type dialogue interface {
	AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta), opts ...CallOpt) (string, error)
}

// BudgetGate 是成本软限额闸门（*cost.Guard 实现）。它只拦**主动型**工作：
// 心跳巡检与智能任务到点时若已超预算，直接静默跳过，不调用模型。
// 用户对话与计划步骤不经过此闸门（那是用户主动发起的工作）。
type BudgetGate interface {
	ProactiveAllowed(what string) (bool, string)
}

// TaskRunner 执行智能任务（KindAgent）。会话 key 固定为 task，
// 智能任务之间串行、与用户对话各会话互不交叉。
type TaskRunner struct {
	Ask           dialogue
	HeartbeatFile string // HEARTBEAT.md 路径（心跳任务读取）
	QuietHours    string // "23:00-07:00"（支持跨零点）；空 = 关闭静默
	Notifiers     []Notifier
	Budget        BudgetGate       // nil = 不做预算拦截
	Now           func() time.Time // 可注入时钟（测试用）；nil 用 time.Now
}

// Run 执行一条到点的智能任务：构造 prompt → agent 决策 → 按输出协议投递。
// 全程留痕（source=task，session=task）。
func (r *TaskRunner) Run(ctx context.Context, t task.Task) {
	body, push, err := r.Prepare(ctx, t)
	if err != nil {
		if ctx.Err() == nil {
			logx.Printf(ctx, "[智能任务] 执行失败: %v", err)
			trace.Record(ctx, "error", "智能任务执行失败：%v", err)
		}
		return
	}
	if push {
		r.deliver(ctx, t, body)
	}
}

// Prepare 将模型决策和投递分开；结果持久化后重试通知不再重复运行模型/工具。
func (r *TaskRunner) Prepare(ctx context.Context, t task.Task) (string, bool, error) {
	if r.Budget != nil {
		if ok, reason := r.Budget.ProactiveAllowed(r.taskLabel(t)); !ok {
			// 超预算不再启动模型：省的是真金白银，留一条 silent 通知让用户查得到。
			logx.Printf(ctx, "[智能任务] 预算闸门：%s", reason)
			trace.Record(ctx, "budget", "%s——本次跳过模型调用", reason)
			return "预算闸门：" + reason, false, nil
		}
	}

	prompt := r.buildPrompt(t)
	answer, err := r.Ask.AskWith(WithSessionKey(ctx, SessionTask), prompt, nil, nil, WithoutAutoSkill())
	if err != nil {
		return "", false, err
	}

	body, push := parseAgentOutput(answer)
	switch {
	case !push:
		logx.Printf(ctx, "[智能任务] agent 决策 [静默]，不打扰用户")
		trace.Record(ctx, "task", "agent 输出 [静默]：本次不推送")
	case r.inQuiet(r.now()):
		logx.Printf(ctx, "[智能任务] 静默时段（%s），只记轨迹不推送", r.QuietHours)
		trace.Record(ctx, "task", "静默时段（%s）：内容已记录，不推送", r.QuietHours)
	case strings.TrimSpace(body) == "":
		trace.Record(ctx, "task", "agent 推送正文为空，跳过投递")
	default:
		return body, true, nil
	}
	return body, false, nil
}

// buildPrompt 构造智能任务 prompt：任务指令 + 当前时间 + 输出协议说明；
// 心跳任务额外附上 HEARTBEAT.md 检查清单。
func (r *TaskRunner) buildPrompt(t task.Task) string {
	var b strings.Builder
	b.WriteString("你是用户的私人生活管家小五（Friday），现在执行一条到点的定时智能任务，请自主决策并行动（可以像平时一样使用你的全部工具）。\n")
	b.WriteString(fmt.Sprintf("当前时间：%s\n", r.now().Format("2006-01-02 15:04:05 Monday")))
	b.WriteString("任务指令：" + t.Text + "\n")
	if t.Heartbeat {
		b.WriteString("\n心跳检查清单（HEARTBEAT.md 内容）：\n")
		if data, err := os.ReadFile(r.HeartbeatFile); err == nil {
			b.WriteString(strings.TrimSpace(string(data)))
		} else {
			b.WriteString("（清单文件读取失败：" + err.Error() + "，请按常识判断是否有值得告知用户的事）")
		}
		b.WriteString("\n")
	}
	b.WriteString(`
输出协议（必须遵守）：回答的第一行只能是 [推送] 或 [静默] 二者之一。
- [推送]：本次需要告知用户。第一行写 [推送]，从第二行起写给用户的内容（会被投递到各通道）。
- [静默]：本次无需打扰用户（如一切正常、无到期事项、不值得打断）。第一行只写 [静默]，后面可简述原因（仅记录轨迹）。
`)
	return b.String()
}

// parseAgentOutput 解析输出协议：首行 [推送] → (正文, true)；[静默] → ("", false)。
// 缺协议标记时保守按推送处理（避免丢内容），由调用方补记轨迹。
func parseAgentOutput(answer string) (body string, push bool) {
	lines := strings.SplitN(strings.TrimSpace(answer), "\n", 2)
	first := strings.TrimSpace(lines[0])
	switch {
	case strings.HasPrefix(first, "[推送]"):
		rest := strings.TrimSpace(strings.TrimPrefix(first, "[推送]"))
		if len(lines) == 2 {
			if rest != "" {
				body = rest + "\n" + lines[1]
			} else {
				body = strings.TrimSpace(lines[1])
			}
		} else {
			body = rest
		}
		return body, true
	case strings.HasPrefix(first, "[静默]"):
		return "", false
	default:
		return strings.TrimSpace(answer), true
	}
}

// deliver 把正文投递到通知目标：任务指定 Channel 时只投该通道，
// 否则投全部。无可用目标时记录轨迹（内容不丢）。
func (r *TaskRunner) deliver(ctx context.Context, t task.Task, body string) {
	if strings.TrimSpace(body) == "" {
		trace.Record(ctx, "task", "agent 决策推送但正文为空，跳过投递")
		return
	}
	delivered := 0
	for _, n := range r.Notifiers {
		if t.Channel != "" && t.Channel != n.Name() {
			continue
		}
		if err := n.Notify(ctx, body); err != nil {
			logx.Printf(ctx, "[智能任务] 推送到 %s 失败: %v", n.Name(), err)
			trace.Record(ctx, "error", "推送到 %s 失败：%v", n.Name(), err)
			continue
		}
		delivered++
		trace.Record(ctx, "task", "已推送到 %s", n.Name())
	}
	if delivered == 0 {
		trace.Record(ctx, "task", "无可用推送通道（任务通道=%q），内容仅记录于轨迹", t.Channel)
	}
}

// inQuiet 判断 t 是否落在静默时段 "HH:MM-HH:MM"（支持跨零点，如 23:00-07:00）。
// 解析失败视为不静默。
func (r *TaskRunner) inQuiet(t time.Time) bool {
	spec := strings.TrimSpace(r.QuietHours)
	if spec == "" {
		return false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, err1 := time.Parse("15:04", strings.TrimSpace(parts[0]))
	end, err2 := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	cur := time.Date(0, 1, 1, t.Hour(), t.Minute(), 0, 0, time.UTC)
	s := time.Date(0, 1, 1, start.Hour(), start.Minute(), 0, 0, time.UTC)
	e := time.Date(0, 1, 1, end.Hour(), end.Minute(), 0, 0, time.UTC)
	if s.Before(e) || s.Equal(e) {
		return !cur.Before(s) && cur.Before(e)
	}
	// 跨零点：23:00-07:00 → cur >= 23:00 || cur < 07:00
	return !cur.Before(s) || cur.Before(e)
}

func (r *TaskRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// taskLabel 给预算闸门一句人能读懂的类别名（原因文本会进轨迹与通知）。
func (r *TaskRunner) taskLabel(t task.Task) string {
	if t.Heartbeat {
		return "心跳巡检"
	}
	return "智能任务"
}
