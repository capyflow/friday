// plans.go 提供计划模式的管理接口：查看、确认（可先改再确认）、暂停/继续/取消、
// 失败步骤重试或跳过。
//
// 确认通道刻意不复用危险操作的审批队列：那条队列在非 confirm 权限档下会自动放行
// （acceptEdits/fullAccess），拿它做计划确认会出现"权限模式一开就没人确认计划"的
// 安全倒退；而计划确认是"用户要不要让小五这么做"，与权限模式无关，必须永远问一次。
package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/plan"
)

// registerPlanRoutes 挂载计划接口（未启用计划模式时跳过）。
func (s *Server) registerPlanRoutes(root *vhttp.VortexHttpRouterGroup) {
	if s.plans == nil || s.planRunner == nil {
		return
	}
	root.AddRouter([]string{http.MethodGet}, "/api/plans", s.wrap(s.handleListPlans))
	root.AddRouter([]string{http.MethodGet}, "/api/plans/:id", s.wrap(s.handleGetPlan))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/confirm", s.wrap(s.handleConfirmPlan))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/pause", s.wrap(s.handlePausePlan))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/resume", s.wrap(s.handleResumePlan))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/cancel", s.wrap(s.handleCancelPlan))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/steps/:n/retry", s.wrap(s.handleRetryStep))
	root.AddRouter([]string{http.MethodPost}, "/api/plans/:id/steps/:n/skip", s.wrap(s.handleSkipStep))
}

func (s *Server) handleListPlans(ctx *vhttp.Context) error {
	status := strings.TrimSpace(ctx.GinContext().Query("status"))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"plans": s.plans.List(status),
		"error": errText(s.plans.Err()),
	})
}

func (s *Server) handleGetPlan(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	p, err := s.plans.Get(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"plan": p, "running": s.planRunner.Running(id),
	})
}

// handleConfirmPlan 确认执行：approve=false 即取消；带 steps 时先按用户编辑的版本
// 替换步骤清单（保留已完成的步骤，只能改还没做的）。
func (s *Server) handleConfirmPlan(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	var body struct {
		Approve bool `json:"approve"`
		Steps   []struct {
			Title    string   `json:"title"`
			Detail   string   `json:"detail"`
			DoneWhen string   `json:"doneWhen"`
			Tools    []string `json:"tools"`
		} `json:"steps"`
	}
	if err := ctx.UnmarshalBody(&body); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 {approve: bool, steps?: [...]}"})
	}
	p, err := s.plans.Get(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if p.State.Terminal() {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "计划已结束（" + string(p.State) + "），不能重复确认"})
	}

	if !body.Approve {
		if err := s.planRunner.Cancel(id); err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
		}
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "已取消这份计划"})
	}

	confirmed := true
	now := time.Now()
	if _, err := s.plans.Update(id, func(q *plan.Plan) error {
		if len(body.Steps) > 0 {
			pending := 0
			for _, st := range q.Steps {
				if st.State == plan.StepPending {
					pending++
				}
			}
			if len(body.Steps) != pending {
				return fmt.Errorf("编辑后的步骤数（%d）与待执行步骤数（%d）不一致", len(body.Steps), pending)
			}
			i := 0
			for j := range q.Steps {
				if q.Steps[j].State != plan.StepPending {
					continue
				}
				q.Steps[j].Title = strings.TrimSpace(body.Steps[i].Title)
				q.Steps[j].Detail = strings.TrimSpace(body.Steps[i].Detail)
				q.Steps[j].DoneWhen = strings.TrimSpace(body.Steps[i].DoneWhen)
				q.Steps[j].Tools = append([]string(nil), body.Steps[i].Tools...)
				i++
			}
		}
		q.Confirmed = &confirmed
		q.ConfirmedAt = &now
		q.State = plan.StateRunning
		return nil
	}); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}

	if err := s.planRunner.Start(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "已确认，正在按步骤执行",
		"plan":    mustPlan(s, id),
	})
}

func (s *Server) handlePausePlan(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	if err := s.planRunner.Pause(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "已暂停", "plan": mustPlan(s, id)})
}

func (s *Server) handleResumePlan(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	p, err := s.plans.Get(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if p.State != plan.StatePaused {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "只有暂停中的计划可以继续（当前 " + string(p.State) + "）"})
	}
	if err := s.planRunner.Start(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "已继续执行", "plan": mustPlan(s, id)})
}

func (s *Server) handleCancelPlan(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	if err := s.planRunner.Cancel(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "已取消", "plan": mustPlan(s, id)})
}

func (s *Server) handleRetryStep(ctx *vhttp.Context) error {
	id, n, err := planStepParams(ctx)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if err := s.planRunner.RetryStep(id, n); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": fmt.Sprintf("第 %d 步已排队执行", n), "plan": mustPlan(s, id)})
}

func (s *Server) handleSkipStep(ctx *vhttp.Context) error {
	id, n, err := planStepParams(ctx)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if err := s.planRunner.SkipStep(id, n); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": fmt.Sprintf("已跳过第 %d 步", n), "plan": mustPlan(s, id)})
}

func planStepParams(ctx *vhttp.Context) (string, int, error) {
	id := ctx.GinContext().Param("id")
	raw := ctx.GinContext().Param("n")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("步骤序号非法: %s", raw)
	}
	return id, n, nil
}

// errText 把 error 摊平成响应字段（nil 给空串，前端据此决定是否显示）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mustPlan(s *Server, id string) any {
	p, err := s.plans.Get(id)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	return p
}
