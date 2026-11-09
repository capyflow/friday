// runs.go 是「运行」页的聚合接口：把 ACP 当前轮次、计划、目标、定时任务、
// 待审批与失败项收成一份快照，前端一屏看到"现在在跑什么、有什么卡住了"。
// 它只读现有管理器，不引入新状态；动作仍走各自原有接口（stop/pause/cancel/decide）。
package admin

import (
	"sort"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/task"
)

type acpRunView struct {
	ThreadID    string    `json:"threadId"`
	Title       string    `json:"title"`
	Workspace   string    `json:"workspace,omitempty"`
	Agent       string    `json:"agent"`
	Cwd         string    `json:"cwd,omitempty"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	ContextUsed int       `json:"contextUsed,omitempty"`
	ContextSize int       `json:"contextSize,omitempty"`
}

type planRunView struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Running      bool      `json:"running"`
	CurrentStep  int       `json:"currentStep,omitempty"`
	TotalSteps   int       `json:"totalSteps"`
	CurrentTitle string    `json:"currentTitle,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

type goalRunView struct {
	ID          string    `json:"id"`
	Text        string    `json:"text"`
	Status      string    `json:"status"`
	Running     bool      `json:"running"`
	UsedTokens  int64     `json:"usedTokens"`
	StepsLimit  int       `json:"stepsLimit"`
	TokensLimit int64     `json:"tokensLimit"`
	Next        time.Time `json:"next,omitempty"`
}

type taskRunView struct {
	ID     string    `json:"id"`
	Text   string    `json:"text"`
	Kind   string    `json:"kind"`
	Status string    `json:"status"`
	Next   time.Time `json:"next,omitempty"`
}

type failureRunView struct {
	Kind   string    `json:"kind"` // notification | acp
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// handleRuns 返回运行看板快照。各管理器可能未装配（计划/目标/ACP 可关闭），
// 缺哪块就回空数组，不把"未配置"当成"正在运行"。
func (s *Server) handleRuns(ctx *vhttp.Context) error {
	out := map[string]any{
		"generatedAt": time.Now(),
		"acp":         []acpRunView{},
		"plans":       []planRunView{},
		"goals":       []goalRunView{},
		"tasks":       []taskRunView{},
		"approvals":   []approval.Item{},
		"failures":    []failureRunView{},
		"counts":      map[string]int{},
	}
	acpRuns := []acpRunView{}
	planRuns := []planRunView{}
	goalRuns := []goalRunView{}
	taskRuns := []taskRunView{}
	failures := []failureRunView{}

	if s.acp != nil {
		names := map[string]string{}
		for _, ws := range s.acp.WorkspaceList() {
			names[ws.ID] = ws.Name
		}
		for _, th := range s.acp.List() {
			if th.State != acp.StateRunning && th.State != acp.StateInterrupted {
				continue
			}
			title := th.Title
			if title == "" {
				title = "新会话"
			}
			acpRuns = append(acpRuns, acpRunView{
				ThreadID: th.ID, Title: title, Workspace: names[th.WorkspaceID],
				Agent: th.Agent, Cwd: th.Cwd, State: th.State,
				StartedAt: th.TurnStartedAt, ContextUsed: th.ContextUsed, ContextSize: th.ContextSize,
			})
			if th.State == acp.StateInterrupted {
				failures = append(failures, failureRunView{
					Kind: "acp", ID: th.ID, Title: title,
					Detail: "上次运行被中断，打开会话发下一句即可重接（或按降级策略重置上下文）",
					At:     th.UpdatedAt,
				})
			}
		}
		sort.Slice(acpRuns, func(i, j int) bool {
			if acpRuns[i].State != acpRuns[j].State {
				rank := func(s string) int {
					switch s {
					case acp.StateRunning:
						return 0
					default:
						return 1
					}
				}
				return rank(acpRuns[i].State) < rank(acpRuns[j].State)
			}
			return acpRuns[i].StartedAt.After(acpRuns[j].StartedAt)
		})
		if len(acpRuns) > 10 {
			acpRuns = acpRuns[:10]
		}
	}

	if s.plans != nil {
		for _, p := range s.plans.List("") {
			if !p.Active() {
				continue
			}
			cur, title := planCurrentStep(p)
			running := false
			if s.planRunner != nil {
				running = s.planRunner.Running(p.ID)
			}
			planRuns = append(planRuns, planRunView{
				ID: p.ID, Title: p.Title, State: string(p.State), Running: running,
				CurrentStep: cur, TotalSteps: len(p.Steps), CurrentTitle: title, UpdatedAt: p.UpdatedAt,
			})
		}
		sort.Slice(planRuns, func(i, j int) bool { return planRuns[i].UpdatedAt.After(planRuns[j].UpdatedAt) })
		if len(planRuns) > 10 {
			planRuns = planRuns[:10]
		}
	}

	if s.goalRunner != nil && s.engine != nil {
		for _, t := range s.engine.List() {
			if !t.Goal {
				continue
			}
			v := s.goalView(t)
			if !v.Running && t.Status != task.StatusPaused {
				continue
			}
			goalRuns = append(goalRuns, goalRunView{
				ID: t.ID, Text: t.Text, Status: t.Status, Running: v.Running,
				UsedTokens: v.UsedTokens, StepsLimit: v.StepsLimit, TokensLimit: v.TokensLimit,
				Next: t.Next,
			})
		}
		sort.Slice(goalRuns, func(i, j int) bool {
			if goalRuns[i].Running != goalRuns[j].Running {
				return goalRuns[i].Running
			}
			return goalRuns[i].Next.Before(goalRuns[j].Next)
		})
		if len(goalRuns) > 10 {
			goalRuns = goalRuns[:10]
		}
	}

	if s.engine != nil {
		for _, t := range s.engine.List() {
			if t.Goal || t.Heartbeat {
				continue
			}
			if t.Status != task.StatusActive && t.Status != task.StatusPaused {
				continue
			}
			taskRuns = append(taskRuns, taskRunView{
				ID: t.ID, Text: t.Text, Kind: string(t.Kind), Status: t.Status, Next: t.Next,
			})
		}
		sort.Slice(taskRuns, func(i, j int) bool {
			// 已暂停的排后面，其余按下次触发时间；没有 Next 的排最后。
			if (taskRuns[i].Status == task.StatusPaused) != (taskRuns[j].Status == task.StatusPaused) {
				return taskRuns[i].Status != task.StatusPaused
			}
			if taskRuns[i].Next.IsZero() != taskRuns[j].Next.IsZero() {
				return !taskRuns[i].Next.IsZero()
			}
			return taskRuns[i].Next.Before(taskRuns[j].Next)
		})
		if len(taskRuns) > 8 {
			taskRuns = taskRuns[:8]
		}
	}

	approvals := []approval.Item{}
	if s.approver != nil {
		approvals = s.approver.Pending()
	}
	if s.notifications != nil {
		for _, n := range s.notifications.List() {
			if n.Status != "failed" {
				continue
			}
			title := n.Task.Text
			if title == "" {
				title = n.Text
			}
			failures = append(failures, failureRunView{
				Kind: "notification", ID: n.ID, Title: title, Detail: n.LastError, At: n.UpdatedAt,
			})
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].At.After(failures[j].At) })
	if len(failures) > 8 {
		failures = failures[:8]
	}

	out["acp"] = acpRuns
	out["plans"] = planRuns
	out["goals"] = goalRuns
	out["tasks"] = taskRuns
	out["approvals"] = approvals
	out["failures"] = failures
	out["counts"] = map[string]int{
		"acp": len(acpRuns), "plans": len(planRuns), "goals": len(goalRuns),
		"tasks": len(taskRuns), "approvals": len(approvals), "failures": len(failures),
	}
	return ctx.JsonResponse(vhttp.Codes.Success, out)
}

// planCurrentStep 返回当前正在执行（或下一个待执行）的步骤序号与标题。
func planCurrentStep(p plan.Plan) (int, string) {
	for _, st := range p.Steps {
		if st.State == plan.StepRunning {
			return st.N, st.Title
		}
	}
	for _, st := range p.Steps {
		if st.State == plan.StepPending {
			return st.N, st.Title
		}
	}
	return 0, ""
}
