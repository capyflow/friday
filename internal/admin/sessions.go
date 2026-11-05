// sessions.go 是网页会话的管理接口：列出主会话与工作会话、新建/改名/删除工作会话。
// 会话本体在 session.json（框架的 sessionstore），这里的增删改只管 Friday 自己
// 那份登记（sessions.go in internal/web），删除时把历史与调用轨迹一并带走。
package admin

import (
	"strings"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

// handleListSessions 返回全部网页会话（主会话 + 工作会话 + 后台入口会话），
// 带显示名与归属，供聊天页的会话栏与设置页的会话列表用。
func (s *Server) handleListSessions(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": []any{}})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": s.chat.ListSessions(ctx.Context())})
}

// handleSessionCreate 新建一条工作会话：{"title":"可选"}。
// 名字留空就等第一条提问自动取名，跟工作区里建会话一个口径。
func (s *Server) handleSessionCreate(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	var req struct {
		Title string `json:"title"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body = 不起名
	session, err := s.chat.CreateWorkSession(ctx.Context(), req.Title)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"session": session})
}

// handleSessionRename 改工作会话的名字：{"title":"…"}，留空回到按首条提问自动取名。
func (s *Server) handleSessionRename(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 {\"title\": \"…\"}"})
	}
	session, err := s.chat.RenameWorkSession(ctx.Context(), ctx.GinContext().Param("key"), req.Title)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"session": session})
}

// handleSessionDelete 删除一条工作会话（历史与调用轨迹一并清除）。
// 主会话删不掉：那是生活模式唯一的对话，清内容请走「清除上下文」。
func (s *Server) handleSessionDelete(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	key := strings.TrimSpace(ctx.GinContext().Param("key"))
	if err := s.chat.DeleteWorkSession(key); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除会话 " + key})
}
