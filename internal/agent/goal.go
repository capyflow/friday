// goal.go 实现"目标模式"：一条被标记为 goal 的智能任务到点后不是一次问答，
// 而是在隔离会话里多轮自主推进，直到模型判定达成、步数用满或 token 预算触顶。
//
// 与计划模式的区别：计划是"先把清单摊开给用户确认再照做"（用户已审阅每一步），
// 目标是"给一个结果，小五自己决定怎么走"（只有结论给用户）。两者都不占用主会话，
// 避免长任务把用户聊天堵在 per-key 锁后面。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// SessionGoal 是目标运行的会话 key 前缀（每个目标一条，重启后仍可回看）。
const SessionGoal = "goal-"

// goalStartDelay 给聊天里即时发起的目标留一点缓冲：先回一句确认，再让排程器点火。
const goalStartDelay = 3 * time.Second

// LaunchGoal 立刻发起一个目标（聊天"目标"模式）：不指定 ID、不排期到未来，
// 交给任务引擎按"几秒后的单次 agent 任务"落盘——于是重启后它也还在，
// 执行仍走通知中心的 Resolver → GoalRunner.Prepare，与定时目标完全同一条路。
func LaunchGoal(eng *task.Engine, text string) (task.Task, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return task.Task{}, errors.New("目标内容不能为空")
	}
	return eng.Schedule(task.Task{
		Kind: task.KindAgent, Text: text, Goal: true,
		When: time.Now().Add(goalStartDelay),
	})
}

// 目标推进的软预算默认值（任务未显式配置时）。
const (
	defaultGoalSteps  = 8
	defaultGoalTokens = int64(120000)
)

// GoalRunner 执行目标。
type GoalRunner struct {
	Ask             dialogue
	Notifiers       []Notifier
	Budget          BudgetGate
	MaxConcurrent   int           // 同时跑几个目标（默认 1：目标轮次长、成本大）
	DefaultSteps    int           // 未配 maxSteps 时的步数上限
	DefaultTokens   int64         // 未配 maxTokens 时的 token 预算
	StepIterations  int           // 单步允许的工具轮数（0 = 用 Agent 默认）
	SlotWaitTimeout time.Duration // 抢不到并发槽位时的等待上限

	mu       sync.Mutex
	active   map[string]struct{}
	stopping map[string]bool
	sem      chan struct{}
}

// NewGoalRunner 创建执行器；MaxConcurrent<=0 时按 1。
func NewGoalRunner(ask dialogue, notifiers []Notifier) *GoalRunner {
	r := &GoalRunner{
		Ask: ask, Notifiers: notifiers,
		DefaultSteps: defaultGoalSteps, DefaultTokens: defaultGoalTokens,
		SlotWaitTimeout: 5 * time.Second,
		active:          map[string]struct{}{}, stopping: map[string]bool{},
	}
	n := r.MaxConcurrent
	if n <= 0 {
		n = 1
	}
	r.sem = make(chan struct{}, n)
	return r
}

// ConfigureSlots 在装配后重设并发上限（main 读 config 后调用一次）。
func (r *GoalRunner) ConfigureSlots(n int) {
	if n <= 0 {
		n = 1
	}
	r.MaxConcurrent = n
	r.sem = make(chan struct{}, n)
}

// Prepare 推进一个目标直到得出可供用户看的结论；决策与投递分离，
// 通知中心重试时不会重跑模型（与智能任务同一形态）。
func (r *GoalRunner) Prepare(ctx context.Context, t task.Task) (string, bool, error) {
	if r.Budget != nil {
		if ok, reason := r.Budget.ProactiveAllowed("目标运行"); !ok {
			trace.Record(ctx, "budget", "%s——目标未启动", reason)
			return "预算闸门：" + reason, false, nil
		}
	}
	slot, release, err := r.acquireSlot(ctx)
	if err != nil {
		return "", false, err
	}
	if !slot {
		msg := fmt.Sprintf("同时运行的目标已达上限（%d），本次目标跳过", r.MaxConcurrent)
		trace.Record(ctx, "goal_run", "%s", msg)
		return msg, false, nil
	}
	defer release()

	r.markActive(t.ID, true)
	defer r.markActive(t.ID, false)

	goalKey := GoalSessionKey(t.ID)
	runLabel := "goal:" + t.ID
	maxSteps := t.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.DefaultSteps
	}
	tokenBudget := t.MaxTokens
	if tokenBudget <= 0 {
		tokenBudget = r.DefaultTokens
	}

	var progress []string
	var totalIn, totalOut int64
	outcome := "budget_exhausted"
	final := ""

	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			outcome = "canceled"
			break
		}
		if r.stopRequested(t.ID) {
			outcome = "canceled"
			break
		}
		if r.Budget != nil {
			if ok, reason := r.Budget.ProactiveAllowed("目标运行"); !ok {
				trace.Record(ctx, "budget", "%s——目标在第 %d 步停下", reason, step)
				outcome = "budget_exhausted"
				break
			}
		}
		if tokenBudget > 0 && totalIn+totalOut >= tokenBudget {
			note := fmt.Sprintf("第 %d 步前已用 %d token，达到目标预算 %d，按步边界停下",
				step, totalIn+totalOut, tokenBudget)
			trace.Record(ctx, "budget", "%s", note)
			outcome, final = "budget_exhausted", note
			break
		}

		sctx := logx.WithID(ctx, logx.NextID())
		sctx = WithSessionKey(sctx, goalKey)
		sctx = WithOrigin(sctx, OriginTask)
		sctx = WithRun(sctx, runLabel)
		trace.Begin(logx.IDFrom(sctx), "goal", goalKey, t.Text)
		trace.Record(sctx, "goal_step", "第 %d/%d 步开始", step, maxSteps)

		beforeIn, beforeOut := r.runTokens(runLabel)
		answer, err := r.Ask.AskWith(sctx, r.stepPrompt(t, step, maxSteps, progress), nil, nil, r.stepOpts(t)...)
		afterIn, afterOut := r.runTokens(runLabel)
		stepIn, stepOut := diff(beforeIn, afterIn), diff(beforeOut, afterOut)
		totalIn += stepIn
		totalOut += stepOut

		if err != nil {
			trace.End(logx.IDFrom(sctx), err.Error())
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				outcome = "canceled"
				break
			}
			// 单步失败不判死：记下来，下一步带着失败原因继续试。
			progress = append(progress, fmt.Sprintf("第 %d 步失败：%v", step, err))
			continue
		}
		trace.End(logx.IDFrom(sctx), answer)

		state, body := parseGoalAnswer(answer)
		progress = append(progress, fmt.Sprintf("第 %d 步：%s", step, logx.Truncate(body, 300)))
		if state == "achieved" || state == "stalled" {
			outcome, final = state, body
			break
		}
		// [继续]：带着这一步的进展进入下一步；最后一步仍是 [继续] 时，
		// 把它的进展作为"告一段落"的结论返回（比空手收尾更有信息量）。
		final, outcome = body, "budget_exhausted"
	}

	summary := r.summarize(t, outcome, final, maxSteps, totalIn+totalOut)
	if outcome == "achieved" {
		trace.Record(ctx, "goal_run", "目标达成：%s", logx.Truncate(summary, 200))
	} else {
		trace.Record(ctx, "goal_run", "目标结束（%s）", outcome)
	}
	// 只有"达成"才主动打扰用户；受阻/超预算只留档，避免半夜反复推送坏消息。
	return summary, outcome == "achieved", nil
}

// diff 差值（账本可能被其它会话并发累加，负数按 0 计）。
func diff(before, after int64) int64 {
	if after <= before {
		return 0
	}
	return after - before
}

// stepOpts 单步提问选项：钉住技能（若配置）、放宽轮数、关掉自动匹配。
func (r *GoalRunner) stepOpts(t task.Task) []CallOpt {
	opts := []CallOpt{WithoutAutoSkill()}
	if t.Skill != "" {
		opts = append(opts, WithSkillName(t.Skill))
	}
	if r.StepIterations > 0 {
		opts = append(opts, WithMaxIterations(r.StepIterations))
	}
	return opts
}

// stepPrompt 构造单步指令。目标会话是独立的（不含用户聊天），因此把目标原文、
// 已推进的过程与"什么时候该停"都写清楚。
func (r *GoalRunner) stepPrompt(t task.Task, step, maxSteps int, progress []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("你在推进用户的一个人生活管家目标（第 %d 步 / 最多 %d 步）。\n", step, maxSteps))
	b.WriteString("目标：" + t.Text + "\n")
	b.WriteString(fmt.Sprintf("当前时间：%s\n", time.Now().Format("2006-01-02 15:04:05 Monday")))
	if len(progress) > 0 {
		b.WriteString("已经做过的事：\n" + strings.Join(progress, "\n") + "\n")
	}
	b.WriteString(`
这一步只推进目标的一小步（可以用你的全部工具，但别做与目标无关的事）。
回答的第一行必须是下列标记之一：
- [继续] 目标还没完成：第一行写 [继续]，后面简述这一步做了什么、下一步打算做什么。
- [达成] 目标已经完成：第一行写 [达成]，后面写给用户的结论（用户会收到这条消息）。
- [受阻] 缺少必要信息或需要用户决定：第一行写 [受阻]，后面说清楚卡在哪、需要用户做什么（不推送，等用户主动看）。
`)
	return b.String()
}

// parseGoalAnswer 解析目标推进协议。缺标记按"继续"处理（保守：不轻易宣布完成）。
func parseGoalAnswer(answer string) (state, body string) {
	lines := strings.SplitN(strings.TrimSpace(answer), "\n", 2)
	first := strings.TrimSpace(lines[0])
	body = ""
	if len(lines) == 2 {
		body = strings.TrimSpace(lines[1])
	}
	switch {
	case strings.HasPrefix(first, "[达成]"):
		return "achieved", strings.TrimSpace(strings.TrimPrefix(first, "[达成]")) + tail(body)
	case strings.HasPrefix(first, "[受阻]"):
		return "stalled", strings.TrimSpace(strings.TrimPrefix(first, "[受阻]")) + tail(body)
	default:
		return "continue", strings.TrimSpace(strings.TrimPrefix(first, "[继续]")) + tail(body)
	}
}

func tail(body string) string {
	if body == "" {
		return ""
	}
	return "\n" + body
}

func (r *GoalRunner) summarize(t task.Task, outcome, final string, maxSteps int, tokens int64) string {
	switch outcome {
	case "achieved":
		return "目标已完成：" + t.Text + "\n" + final
	case "stalled":
		return "目标需要你决定：" + t.Text + "\n" + final
	case "canceled":
		return "目标已停止：" + t.Text + "（进行中的这一步不再继续）"
	default:
		if final == "" {
			final = "没有新的进展说明"
		}
		return fmt.Sprintf("目标暂告一段落：%s\n%s\n（用了 %d 步、%d token；可在「自动化」页继续或调整预算）",
			t.Text, final, maxSteps, tokens)
	}
}

// Stop 请求在步边界停止某目标（不打断正在生成的那一步的模型调用之外的动作）。
func (r *GoalRunner) Stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopping[id] = true
}

// Running 报告目标是否正在推进。
func (r *GoalRunner) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[id]
	return ok
}

func (r *GoalRunner) markActive(id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.active[id] = struct{}{}
		return
	}
	delete(r.active, id)
	delete(r.stopping, id)
}

func (r *GoalRunner) stopRequested(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopping[id]
}

// acquireSlot 抢并发槽位：拿不到就等一小会儿，仍拿不到则本次跳过（不排队堆叠，
// 否则一个长目标会让后续触发点全部堵在通知中心的重试队列里）。
func (r *GoalRunner) acquireSlot(ctx context.Context) (bool, func(), error) {
	wait := r.SlotWaitTimeout
	if wait <= 0 {
		wait = 5 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r.sem <- struct{}{}:
		return true, func() { <-r.sem }, nil
	case <-timer.C:
		return false, func() {}, nil
	case <-ctx.Done():
		return false, func() {}, ctx.Err()
	}
}

// runTokens 读该目标在账本里的累计 token（与"运行状态"页同一口径）。
func (r *GoalRunner) runTokens(runLabel string) (int64, int64) {
	days, _ := usage.Summary()
	var in, out int64
	for _, d := range days {
		if dim, ok := d.ByRun[runLabel]; ok && dim != nil {
			in += dim.In
			out += dim.Out
		}
	}
	return in, out
}

// GoalSessionKey 目标运行的会话 key；ID 为空时返回空串（调用方据此跳过）。
// 必须经过 CurrentKey：SessionPool 的键都是归一化后的形态，
// 而 Brain.Reset("") 会落到默认网页会话上——不拦住就会误清用户对话。
func GoalSessionKey(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return SessionGoal + CurrentKey(id)
}

// ReleaseGoalSession 在目标被删除/归档时释放其内存会话（轨迹清理由调用方负责）。
// SessionPool 只增不减，不释放会随目标数量线性膨胀。
func (b *Brain) ReleaseGoalSession(id string) error {
	key := GoalSessionKey(id)
	if key == "" {
		return errors.New("agent: 目标 ID 为空")
	}
	return b.Reset(key)
}
