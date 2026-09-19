// ext.go 是管理后台的扩展接口（M2–M5 新增）：待审批操作、会话管理、
// MCP 服务器状态、心跳清单编辑。路由挂载见 admin.go 的 Start。
package admin

import (
	"os"
	"strconv"
	"strings"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/mcp"
)

// handleListSessions 返回全部会话概要（key、消息数、最后活跃）。
func (s *Server) handleListSessions(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": []any{}})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"sessions": s.chat.Brain().Pool().List()})
}

// handleClearContext 清除对话上下文：body {"session": "key"} 可选——
// 提供则只清该会话，缺省清默认网页会话。
func (s *Server) handleClearContext(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	var req struct {
		Session string `json:"session"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body 视为清默认网页会话
	key := strings.TrimSpace(req.Session)
	if key == "" {
		key = "web"
	}
	s.chat.ResetContext(key)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "会话 " + key + " 上下文已清除"})
}

// handleListApprovals 返回当前待审批操作与审批模式。
func (s *Server) handleListApprovals(ctx *vhttp.Context) error {
	out := map[string]any{"approvals": []any{}}
	if s.approver != nil {
		out["approvals"] = s.approver.Pending()
		out["mode"] = s.approver.Mode()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, out)
}

// handleDecideApproval 审批一条待处理操作：body {"approve": true|false}。
func (s *Server) handleDecideApproval(ctx *vhttp.Context) error {
	if s.approver == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "审批机制未启用"})
	}
	var req struct {
		Approve bool `json:"approve"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	var id int64
	id, err := strconv.ParseInt(ctx.GinContext().Param("id"), 10, 64)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的审批 ID"})
	}
	if !s.approver.Decide(id, req.Approve) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到待审批操作（可能已超时或已处理）"})
	}
	msg := "已拒绝"
	if req.Approve {
		msg = "已批准"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": msg})
}

// handleMCPStatus 返回 MCP 服务器连接状态。
func (s *Server) handleMCPStatus(ctx *vhttp.Context) error {
	servers := []mcp.Status{}
	if s.mcp != nil {
		servers = s.mcp.StatusList()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"servers": servers})
}

// handleMCPReconnect 手动重连一台 MCP 服务器（断开旧进程并重新拉起握手）。
func (s *Server) handleMCPReconnect(ctx *vhttp.Context) error {
	if s.mcp == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "MCP 未启用"})
	}
	name := ctx.GinContext().Param("name")
	if err := s.mcp.Restart(name); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已重连 " + name})
}

// handleGetHeartbeat 读取心跳检查清单内容。
func (s *Server) handleGetHeartbeat(ctx *vhttp.Context) error {
	content, err := os.ReadFile(s.heartbeatFile)
	msg := ""
	if err != nil {
		msg = "（清单文件尚未创建，保存后将生成）"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"path": s.heartbeatFile, "content": string(content), "note": msg,
	})
}

// handleSetHeartbeat 保存心跳检查清单内容：body {"content": "..."}。
func (s *Server) handleSetHeartbeat(ctx *vhttp.Context) error {
	var req struct {
		Content string `json:"content"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := os.WriteFile(s.heartbeatFile, []byte(req.Content), 0o644); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "保存失败: " + err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "心跳清单已保存"})
}
