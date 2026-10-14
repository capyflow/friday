// run.go 按步执行已确认的计划：一步一次独立提问、一条独立轨迹、一份独立成本，
// 用户在任何时刻都能暂停、取消，或对失败的一步重试/跳过。
package plan

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// Runner 执行计划。零值不可用，请用 NewRunner。
type Runner struct {
	Ask           Asker
	Store         *Store
	MaxIterations int   // 单步的工具轮数上限（0 = 用 Agent 默认）
	PerPlanTokens int64 // 整份计划的 token 软预算（0 = 不限）

	base     context.Context
	mu       sync.Mutex
	active   map[string]context.CancelFunc // 计划 id → 取消函数
	stopping map[string]bool               // 计划在步边界停止
}

// NewRunner 创建执行器。baseCtx 必须是应用级上下文（不能用某次 HTTP 请求的
// ctx——请求结束会误杀后台执行）。
func NewRunner(baseCtx context.Context, asker Asker, store *Store) *Runner {
	return &Runner{
		Ask: asker, Store: store, base: baseCtx,
		active: map[string]context.CancelFunc{}, stopping: map[string]bool{},
	}
}

// Start 开始（或从暂停处继续）执行一个计划。同一计划不会双跑。
func (r *Runner) Start(id string) error {
	r.mu.Lock()
	if _, busy := r.active[id]; busy {
		r.mu.Unlock()
		return fmt.Errorf("该计划正在执行中")
	}
	p, err := r.Store.Get(id)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if p.State.Terminal() {
		r.mu.Unlock()
		return fmt.Errorf("计划已结束（%s），不能再启动", p.State)
	}
	if p.State == StateDraft {
		r.mu.Unlock()
		return fmt.Errorf("计划还没有步骤，先让它产出清单")
	}
	if _, err := r.Store.Update(id, func(q *Plan) error {
		q.State = StateRunning
		q.Error = ""
		return nil
	}); err != nil {
		r.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(r.baseContext())
	r.active[id] = cancel
	r.mu.Unlock()

	go r.loop(ctx, id)
	return nil
}

// Pause 请求停止：有步骤在跑时在步边界停（不掐断正在生成的回答），
// 没有则在当前点置为暂停。
func (r *Runner) Pause(id string) error {
	if _, err := r.Store.Get(id); err != nil {
		return err
	}
	r.mu.Lock()
	cancel, running := r.active[id]
	if running {
		r.stopping[id] = true
	}
	r.mu.Unlock()
	if running {
		cancel() // 循环里的步骤会因 ctx 取消而把该步标记失败并停下
		return nil
	}
	_, err := r.Store.Update(id, func(q *Plan) error {
		if q.State.Terminal() {
			return fmt.Errorf("计划已结束（%s）", q.State)
		}
		q.State = StatePaused
		return nil
	})
	return err
}

// Cancel 取消计划（终态）。
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	if cancel, ok := r.active[id]; ok {
		r.stopping[id] = true
		cancel()
	}
	r.mu.Unlock()
	_, err := r.Store.Update(id, func(q *Plan) error {
		q.State = StateCanceled
		for i := range q.Steps {
			if q.Steps[i].State == StepPending || q.Steps[i].State == StepRunning {
				q.Steps[i].State = StepSkipped
			}
		}
		return nil
	})
	return err
}

// Running 报告该计划是否有执行协程在跑。
func (r *Runner) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[id]
	return ok
}

// RetryStep 把失败/跳过的步骤退回待执行，并在计划未结束时继续跑。
func (r *Runner) RetryStep(id string, n int) error {
	if _, err := r.Store.Update(id, func(q *Plan) error {
		for i := range q.Steps {
			if q.Steps[i].N == n {
				if q.Steps[i].State == StepDone {
					return fmt.Errorf("第 %d 步已完成，无需重试", n)
				}
				q.Steps[i].State = StepPending
				q.Steps[i].Error = ""
				q.Steps[i].Answer = ""
				if q.State.Terminal() {
					q.State = StatePaused
				}
				return nil
			}
		}
		return fmt.Errorf("没有第 %d 步", n)
	}); err != nil {
		return err
	}
	if r.Running(id) {
		return nil
	}
	return r.Start(id)
}

// SkipStep 跳过某一步并继续推进。
func (r *Runner) SkipStep(id string, n int) error {
	if _, err := r.Store.Update(id, func(q *Plan) error {
		for i := range q.Steps {
			if q.Steps[i].N == n {
				if q.Steps[i].State == StepDone {
					return fmt.Errorf("第 %d 步已完成，不能跳过", n)
				}
				q.Steps[i].State = StepSkipped
				return nil
			}
		}
		return fmt.Errorf("没有第 %d 步", n)
	}); err != nil {
		return err
	}
	if r.Running(id) {
		return nil
	}
	return r.Start(id)
}

func (r *Runner) baseContext() context.Context {
	if r.base != nil {
		return r.base
	}
	return context.Background()
}

// loop 是执行主循环：每轮取一个待执行步骤，跑完再判预算/停止/完成。
func (r *Runner) loop(ctx context.Context, id string) {
	defer func() {
		r.mu.Lock()
		delete(r.active, id)
		delete(r.stopping, id)
		r.mu.Unlock()
	}()

	for {
		p, err := r.Store.Get(id)
		if err != nil {
			log.Printf("[计划] 读取 %s 失败: %v", id, err)
			return
		}
		if p.State != StateRunning {
			return
		}
		if ctx.Err() != nil {
			r.finish(id, StatePaused, "已暂停（当前步骤被打断）")
			return
		}
		if r.stoppingRequested(id) {
			r.finish(id, StatePaused, "已在步骤边界暂停")
			return
		}
		if over, used := r.overBudget(id); over {
			r.finish(id, StatePaused,
				fmt.Sprintf("计划 token 预算已用满（%d / %d），在步骤边界暂停；提高预算或降低步骤开销后可继续",
					used, r.PerPlanTokens))
			return
		}
		next := nextPending(p)
		if next == nil {
			r.finish(id, StateDone, "")
			return
		}
		r.runStep(ctx, p, *next)
	}
}

// runStep 执行一步：一步一次提问，失败不阻断后续步骤（用户可重试或跳过）。
func (r *Runner) runStep(ctx context.Context, p *Plan, st Step) {
	if _, err := r.Store.Update(p.ID, func(q *Plan) error {
		for i := range q.Steps {
			if q.Steps[i].N == st.N {
				q.Steps[i].State = StepRunning
				q.Steps[i].StartedAt = time.Now()
				return nil
			}
		}
		return fmt.Errorf("第 %d 步已不存在", st.N)
	}); err != nil {
		log.Printf("[计划] %s 第 %d 步标记执行中失败: %v", p.ID, st.N, err)
		return
	}

	sctx := logx.WithID(ctx, logx.NextID())
	sctx = agent.WithSessionKey(sctx, p.Session)
	sctx = agent.WithOrigin(sctx, agent.OriginPlan)
	sctx = agent.WithRun(sctx, "plan:"+p.ID)
	prompt := r.stepPrompt(p, st)
	trace.Begin(logx.IDFrom(sctx), "plan", p.Session, prompt)
	trace.Record(sctx, "plan_step", "开始第 %d/%d 步：%s", st.N, len(p.Steps), st.Title)

	beforeIn, beforeOut := r.runTokens(p.ID)
	opts := []agent.CallOpt{agent.WithoutAutoSkill()}
	if p.Skill != "" {
		opts = append(opts, agent.WithSkillName(p.Skill))
	}
	if r.MaxIterations > 0 {
		opts = append(opts, agent.WithMaxIterations(r.MaxIterations))
	}
	answer, err := r.Ask.AskWith(sctx, prompt, nil, nil, opts...)
	usedIn, usedOut := r.runTokens(p.ID)
	stepIn, stepOut := usedIn-beforeIn, usedOut-beforeOut
	if stepIn < 0 {
		stepIn = 0
	}
	if stepOut < 0 {
		stepOut = 0
	}
	trace.End(logx.IDFrom(sctx), answerIf(err, answer))

	if err != nil {
		// 被打断（暂停/取消）的一步退回待执行：它没做完，也没做完的副作用可言。
		if errors.Is(err, context.Canceled) || sctx.Err() != nil {
			if _, uerr := r.Store.Update(p.ID, func(q *Plan) error {
				for i := range q.Steps {
					if q.Steps[i].N == st.N {
						q.Steps[i].State = StepPending
						q.Steps[i].StartedAt = time.Time{}
						q.Steps[i].In, q.Steps[i].Out = stepIn, stepOut
						q.In += stepIn
						q.Out += stepOut
						return nil
					}
				}
				return nil
			}); uerr != nil {
				log.Printf("[计划] %s 第 %d 步退回待执行失败: %v", p.ID, st.N, uerr)
			}
			trace.Record(sctx, "pause", "第 %d 步被打断，已退回待执行", st.N)
			return
		}
		log.Printf("[计划] %s 第 %d 步失败: %v", p.ID, st.N, err)
		trace.Record(sctx, "error", "第 %d 步执行失败：%v", st.N, err)
		if _, uerr := r.Store.Update(p.ID, func(q *Plan) error {
			for i := range q.Steps {
				if q.Steps[i].N == st.N {
					q.Steps[i].State = StepFailed
					q.Steps[i].Error = err.Error()
					q.Steps[i].EndedAt = time.Now()
					q.Steps[i].In, q.Steps[i].Out = stepIn, stepOut
					q.In += stepIn
					q.Out += stepOut
					return nil
				}
			}
			return fmt.Errorf("第 %d 步已不存在", st.N)
		}); uerr != nil {
			log.Printf("[计划] %s 第 %d 步失败状态回写失败: %v", p.ID, st.N, uerr)
		}
		return
	}

	if _, uerr := r.Store.Update(p.ID, func(q *Plan) error {
		for i := range q.Steps {
			if q.Steps[i].N == st.N {
				q.Steps[i].State = StepDone
				q.Steps[i].Answer = clip(answer, 1200)
				q.Steps[i].EndedAt = time.Now()
				q.Steps[i].In, q.Steps[i].Out = stepIn, stepOut
				q.In += stepIn
				q.Out += stepOut
				return nil
			}
		}
		return fmt.Errorf("第 %d 步已不存在", st.N)
	}); uerr != nil {
		log.Printf("[计划] %s 第 %d 步结果回写失败: %v", p.ID, st.N, uerr)
	}
}

// stepPrompt 构造单步提问。步骤在独立会话里执行，看不到当前聊天记录，
// 所以把目标、整体思路和"这一步算完成"的条件都写进去。
func (r *Runner) stepPrompt(p *Plan, st Step) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("你在执行一份用户已确认的计划《%s》（共 %d 步）。总目标：%s\n",
		p.Title, len(p.Steps), p.Goal))
	if p.Summary != "" {
		b.WriteString("整体思路：" + p.Summary + "\n")
	}
	if done := r.doneList(p, st.N); done != "" {
		b.WriteString("已完成的步骤：\n" + done + "\n")
	}
	b.WriteString(fmt.Sprintf("\n现在只做第 %d 步：%s\n", st.N, st.Title))
	if st.Detail != "" {
		b.WriteString("怎么做：" + st.Detail + "\n")
	}
	if st.DoneWhen != "" {
		b.WriteString("完成标准：" + st.DoneWhen + "（判断已达成就停下并把结果说清楚）\n")
	}
	b.WriteString("不要顺手执行其它步骤的内容；做完这一步后，用一两句话说明结果与用户需要知道的偏差。\n")
	return b.String()
}

func (r *Runner) doneList(p *Plan, before int) string {
	var lines []string
	for _, s := range p.Steps {
		if s.N >= before {
			continue
		}
		switch s.State {
		case StepDone:
			lines = append(lines, fmt.Sprintf("- 第 %d 步 %s：%s", s.N, s.Title, clip(s.Answer, 160)))
		case StepSkipped:
			lines = append(lines, fmt.Sprintf("- 第 %d 步 %s：已跳过", s.N, s.Title))
		case StepFailed:
			lines = append(lines, fmt.Sprintf("- 第 %d 步 %s：失败（%s）", s.N, s.Title, clip(s.Error, 80)))
		}
	}
	return strings.Join(lines, "\n")
}

func (r *Runner) stoppingRequested(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopping[id]
}

// finish 收口计划状态（已终态或被取消时不覆盖）。
func (r *Runner) finish(id string, state State, note string) {
	if _, err := r.Store.Update(id, func(q *Plan) error {
		if q.State.Terminal() || q.State == StateCanceled {
			return nil
		}
		q.State = state
		if note != "" {
			q.Error = note
		}
		return nil
	}); err != nil {
		log.Printf("[计划] %s 收口失败: %v", id, err)
	}
}

// overBudget 判断该计划在 token 账本上是否已超软预算。
func (r *Runner) overBudget(id string) (bool, int64) {
	if r.PerPlanTokens <= 0 {
		return false, 0
	}
	in, out := r.runTokens(id)
	used := in + out
	return used >= r.PerPlanTokens, used
}

// runTokens 读该计划在账本里的累计 token（输入+输出）。
// 归因来自 usage 的 byRun 维度——成本口径与"运行状态"页一致。
func (r *Runner) runTokens(id string) (int64, int64) {
	days, _ := usage.Summary()
	var in, out int64
	key := "plan:" + id
	for _, d := range days {
		if dim, ok := d.ByRun[key]; ok && dim != nil {
			in += dim.In
			out += dim.Out
		}
	}
	return in, out
}

func nextPending(p *Plan) *Step {
	for i := range p.Steps {
		if p.Steps[i].State == StepPending {
			return &p.Steps[i]
		}
	}
	return nil
}

func answerIf(err error, answer string) string {
	if err != nil {
		return ""
	}
	return answer
}

func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
