package admin

import (
	vhttp "github.com/capyflow/vortexv3/server/http"
	"time"
)

func (s *Server) handleNotifications(ctx *vhttp.Context) error {
	values := s.notifications.ListForTask(ctx.GinContext().Query("taskId"))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"notifications": values, "unread": s.notifications.Unread()})
}

func (s *Server) handleReadNotification(ctx *vhttp.Context) error {
	if err := s.notifications.MarkRead(ctx.GinContext().Param("id")); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已标记为已读"})
}

// handleReadAllNotifications 把通知中心里所有可见未读一次标为已读。
func (s *Server) handleReadAllNotifications(ctx *vhttp.Context) error {
	n, err := s.notifications.MarkAllRead()
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	msg := "已全部标记为已读"
	if n == 0 {
		msg = "没有未读通知"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": msg, "count": n})
}

func (s *Server) handleSnoozeNotification(ctx *vhttp.Context) error {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须包含延后分钟数 minutes"})
	}
	if req.Minutes < 1 || req.Minutes > 7*24*60 {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "延后时间须在1分钟至7天之间"})
	}
	tk, err := s.notifications.Snooze(ctx.GinContext().Param("id"), time.Duration(req.Minutes)*time.Minute, s.engine)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "已安排稍后提醒", "task": tk})
}

func (s *Server) handleRetryNotification(ctx *vhttp.Context) error {
	if err := s.notifications.Retry(ctx.GinContext().Param("id")); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已重新排队；已成功的投递通道会跳过"})
}
