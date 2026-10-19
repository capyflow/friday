package admin

import (
	"github.com/capyflow/friday/internal/tools"
	vhttp "github.com/capyflow/vortexv3/server/http"
	"time"
)

func (s *Server) handleUpdateTask(ctx *vhttp.Context) error {
	var args map[string]any
	if err := ctx.UnmarshalBody(&args); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为JSON对象"})
	}
	tk, err := tools.ParseTaskArgs(args)
	if err == nil {
		tk, err = s.engine.Update(ctx.GinContext().Param("id"), tk)
	}
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"task": tk, "message": "未来任务已更新；已开始的执行保留原记录"})
}

func (s *Server) handlePauseTask(ctx *vhttp.Context) error {
	tk, err := s.engine.Pause(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"task": tk, "message": "未来触发已暂停；已开始的执行继续保留"})
}

func (s *Server) handleResumeTask(ctx *vhttp.Context) error {
	tk, err := s.engine.Resume(ctx.GinContext().Param("id"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"task": tk, "message": "任务已恢复"})
}

func (s *Server) handleUpdateEntry(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	var req struct {
		Text     string     `json:"text"`
		DueAt    *time.Time `json:"dueAt"`
		RemindAt *time.Time `json:"remindAt"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为JSON对象，时间须为带时区RFC3339"})
	}
	entry, ok, err := s.todos.UpdateEntry(ctx.GinContext().Param("id"), req.Text, req.DueAt, req.RemindAt)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到条目"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"entry": entry, "message": "条目已更新"})
}

func (s *Server) handleReconcileEntries(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	if err := s.todos.Reconcile(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "关联提醒已恢复同步"})
}
