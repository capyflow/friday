// Package plan 实现"计划模式"：把一句诉求先转成可审阅的步骤清单，用户在网页确认
// 之后逐步执行——每一步一次独立提问、一条独立轨迹、一份独立成本。
//
// 为什么一步一问，而不是把整个计划塞进一次长对话轮：
//  1. 框架的轮数上限是"一次 Ask 内"的，一次长轮跑不完多步，而耗尽时工具副作用
//     已经发生、推理记录却可能整轮丢失——一步一问把损失半径限制在单步；
//  2. 步骤是天然的确认/暂停/重试粒度，用户看得见"现在在做第几步"；
//  3. 成本可按步归属（byRun 分桶 + 每步 token 记账）。
package plan

import (
	"fmt"
	"strings"
	"time"
)

// State 是计划的生命周期状态。
type State string

const (
	StateDraft    State = "draft"            // 已创建，尚未产出步骤
	StateAwaiting State = "awaiting_confirm" // 步骤已产出，等用户确认
	StateRunning  State = "running"          // 正在按步执行
	StatePaused   State = "paused"           // 用户暂停 / 预算或轮数触顶 / 进程重启中断
	StateDone     State = "completed"        // 全部步骤结束（含被跳过的）
	StateFailed   State = "failed"           // 计划本身无法继续（如规划失败）
	StateCanceled State = "canceled"         // 用户取消
)

// Terminal 表示计划不再推进（可被后台淘汰最旧记录）。
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateCanceled
}

// StepState 是单步状态。
type StepState string

const (
	StepPending StepState = "pending"
	StepRunning StepState = "running"
	StepDone    StepState = "done"
	StepFailed  StepState = "failed"
	StepSkipped StepState = "skipped"
)

// Step 是计划中的一个步骤。
type Step struct {
	N         int       `json:"n"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	DoneWhen  string    `json:"doneWhen,omitempty"`
	Tools     []string  `json:"tools,omitempty"`
	State     StepState `json:"state"`
	Answer    string    `json:"answer,omitempty"`
	Error     string    `json:"error,omitempty"`
	ReqID     string    `json:"reqId,omitempty"` // 该步的轨迹/日志请求 ID
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	In        int64     `json:"in,omitempty"`
	Out       int64     `json:"out,omitempty"`
}

// Plan 是一次计划。
type Plan struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Goal    string `json:"goal"` // 用户原话（规划轮的输入）
	Origin  string `json:"origin"`
	Session string `json:"session"` // 独立会话 key：plan-<id>
	// Dir 是步骤执行时的工作目录（空=用默认目录）。工作区里发起的计划要记住
	// 那个项目目录——否则每一步的文件读写与命令都会落到别处，等于在错地方干活。
	Dir   string `json:"dir,omitempty"`
	Skill string `json:"skill,omitempty"`
	State State  `json:"state"`
	Steps []Step `json:"steps"`

	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ConfirmedAt *time.Time `json:"confirmedAt,omitempty"`
	Confirmed   *bool      `json:"confirmed,omitempty"`
	In          int64      `json:"in"`
	Out         int64      `json:"out"`
	Error       string     `json:"error,omitempty"`
}

// maxSteps / maxTitleLen 是模型产出计划时的硬约束（越界即判计划无效）。
const (
	maxSteps    = 12
	maxTitleLen = 120
)

// Active 表示计划还在推进（不应被记录淘汰）。
func (p *Plan) Active() bool { return !p.State.Terminal() }

// PendingSteps 返回尚未处理的步骤序号。
func (p *Plan) PendingSteps() []int {
	var out []int
	for _, s := range p.Steps {
		if s.State == StepPending {
			out = append(out, s.N)
		}
	}
	return out
}

// Validate 检查计划的不变量（用于加载磁盘数据与接收模型产出）。
func (p *Plan) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("计划缺少 id")
	}
	if len(p.Steps) > maxSteps {
		return fmt.Errorf("计划步骤 %d 条，超过上限 %d 条", len(p.Steps), maxSteps)
	}
	seen := map[int]bool{}
	for i := range p.Steps {
		st := &p.Steps[i]
		if st.N <= 0 || seen[st.N] {
			return fmt.Errorf("第 %d 个步骤序号非法或重复（%d）", i+1, st.N)
		}
		seen[st.N] = true
		if strings.TrimSpace(st.Title) == "" {
			return fmt.Errorf("第 %d 个步骤缺少标题", st.N)
		}
		switch st.State {
		case StepPending, StepRunning, StepDone, StepFailed, StepSkipped:
		case "":
			st.State = StepPending
		default:
			return fmt.Errorf("第 %d 个步骤状态非法: %s", st.N, st.State)
		}
	}
	switch p.State {
	case StateDraft, StateAwaiting, StateRunning, StatePaused, StateDone, StateFailed, StateCanceled:
	case "":
		return fmt.Errorf("计划 %s 缺少状态", p.ID)
	default:
		return fmt.Errorf("计划 %s 状态非法: %s", p.ID, p.State)
	}
	return nil
}

// Normalize 补齐缺省字段并按序号排序，使展示与执行顺序稳定。
func (p *Plan) Normalize(now time.Time) {
	if p.Session == "" {
		p.Session = "plan-" + p.ID
	}
	p.Dir = strings.TrimSpace(p.Dir)
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	for i := range p.Steps {
		if p.Steps[i].State == "" {
			p.Steps[i].State = StepPending
		}
	}
	sortSteps(p.Steps)
}

func sortSteps(steps []Step) {
	for i := 1; i < len(steps); i++ {
		for j := i; j > 0 && steps[j-1].N > steps[j].N; j-- {
			steps[j-1], steps[j] = steps[j], steps[j-1]
		}
	}
}

// newID 生成计划 ID：与任务/待办同一形态，便于口头指代。
func newID(now time.Time) string {
	return fmt.Sprintf("p%d", now.UnixNano()/1000)
}
