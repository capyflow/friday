// sessions.go 是网页会话的只读清单接口：主会话 + 心跳/计划/目标这些后台入口
// 会话，供设置页的会话列表按条清除上下文。曾经的创建/改名/删除工作会话端点
// 已随聊天页入口收敛移除。
package admin

import (
	vhttp "github.com/capyflow/vortexv3/server/http"
)

// handleListSessions 返回全部网页会话（主会话 + 后台入口会话），带归属与活跃时刻，
// 供设置页的会话列表用。
func (s *Server) handleListSessions(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": []any{}})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": s.chat.ListSessions(ctx.Context())})
}
