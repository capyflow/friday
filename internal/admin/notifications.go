package admin

import vhttp "github.com/capyflow/vortexv3/server/http"

func (s *Server) handleNotifications(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"notifications": s.notifications.List()})
}

func (s *Server) handleRetryNotification(ctx *vhttp.Context) error {
	if err := s.notifications.Retry(ctx.GinContext().Param("id")); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已重新排队；已成功的投递通道会跳过"})
}
