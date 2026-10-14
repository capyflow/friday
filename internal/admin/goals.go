// goals.go 提供目标模式的查看与干预接口。
//
// 目标就是带 goal:true 的智能任务，因此增删改、暂停、编辑触发时间全部沿用
// /api/tasks（避免两处真相）；这里只补目标特有的三件事：进度（步数/token）、
// 是否正在跑、以及"在步边界停止当前这次运行"。
package admin

import (
	"net/http"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// registerGoalRoutes 挂载目标接口（目标运行器未装配时跳过）。
func (s *Server) registerGoalRoutes(root *vhttp.VortexHttpRouterGroup) {
	if s.goalRunner == nil || s.engine == nil {
		return
	}
	root.AddRouter([]string{http.MethodGet}, "/api/goals", s.wrap(s.handleListGoals))
	root.AddRouter([]string{http.MethodGet}, s.goalRoute(":id"), s.wrap(s.handleGetGoal))
	root.AddRouter([]string{http.MethodPost}, s.goalRoute(":id/stop"), s.wrap(s.handleStopGoal))
}

func (s *Server) goalRoute(suffix string) string {
	return "/api/goals/" + suffix
}

// goalView 是目标在后台的一行视图。
type goalView struct {
	task.Task
	Running     bool   `json:"running"`
	UsedTokens  int64  `json:"usedTokens"`
	StepsLimit  int    `json:"stepsLimit"`
	TokensLimit int64  `json:"tokensLimit"`
	LastRunID   string `json:"lastRunId,omitempty"`
}

func (s *Server) handleListGoals(ctx *vhttp.Context) error {
	if err := s.engine.Err(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	out := []goalView{}
	for _, t := range s.engine.List() {
		if !t.Goal {
			continue
		}
		out = append(out, s.goalView(t))
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"goals": out})
}

func (s *Server) handleGetGoal(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	t, ok := s.engine.Get(id)
	if !ok || !t.Goal {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到目标 " + id})
	}
	view := s.goalView(t)
	// 该目标最近若干轮的执行轨迹（会话 goal-<id> 即一次目标的历次推进）
	events, _ := trace.Events(80, agent.GoalSessionKey(id))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"goal": view, "events": events,
	})
}

func (s *Server) handleStopGoal(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	t, ok := s.engine.Get(id)
	if !ok || !t.Goal {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到目标 " + id})
	}
	if !s.goalRunner.Running(id) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "该目标当前没有在跑（要停止未来的触发请在「自动化」里暂停或取消任务）"})
	}
	s.goalRunner.Stop(id)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "已请求停止：当前这一步跑完就停下，已产出的结论仍会送达",
	})
}

func (s *Server) goalView(t task.Task) goalView {
	steps := t.MaxSteps
	if steps <= 0 {
		steps = s.goalDefaultSteps()
	}
	tokens := t.MaxTokens
	if tokens <= 0 {
		tokens = s.goalDefaultTokens()
	}
	return goalView{
		Task: t, Running: s.goalRunner.Running(t.ID),
		UsedTokens: goalTokens(t.ID), StepsLimit: steps, TokensLimit: tokens,
	}
}

func (s *Server) goalDefaultSteps() int {
	if s.cfg != nil {
		if n := s.cfg.Snapshot().Cost.Budget.PerGoalSteps; n > 0 {
			return n
		}
	}
	return 0
}

func (s *Server) goalDefaultTokens() int64 {
	if s.cfg != nil {
		if n := s.cfg.Snapshot().Cost.Budget.PerGoalTokens; n > 0 {
			return n
		}
	}
	return 0
}

// goalTokens 读该目标在账本里的累计 token（与运行状态页同一口径）。
func goalTokens(id string) int64 {
	days, _ := usage.Summary()
	label := "goal:" + id
	var total int64
	for _, d := range days {
		if dim, ok := d.ByRun[label]; ok && dim != nil {
			total += dim.In + dim.Out
		}
	}
	return total
}
