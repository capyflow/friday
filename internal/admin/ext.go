// ext.go 是管理后台的扩展接口（M2–M5 新增）：待审批操作、会话管理、
// MCP 服务器状态、心跳清单编辑。路由挂载见 admin.go 的 Start。
package admin

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/capyflow/vortexagent/llm"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// chatHistoryMax 是历史回显的最大条数（老会话只展示最近一段，避免页面过长）。
const chatHistoryMax = 100

// handleChatHistory 返回指定会话（?session=key，缺省 web）的可见对话历史：
// 仅 user/assistant 的文本消息（工具轮次跳过），供打开页面/切换会话时回显。
// 只读查询——不触发 LLM 调用，也不创建空会话。
func (s *Server) handleChatHistory(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"messages": []any{}})
	}
	key := strings.TrimSpace(ctx.GinContext().Query("session"))
	msgs := s.chat.Brain().Pool().History(ctx.Context(), key)
	type item struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	out := make([]item, 0, len(msgs))
	for _, m := range msgs {
		role := string(m.Role)
		if role != "user" && role != "assistant" {
			continue // 工具结果/系统轮次不进对话流
		}
		var text string
		for _, c := range m.Content {
			switch c.Type {
			case llm.ContentText:
				text += c.Text
			case llm.ContentImage:
				text += "[图片]"
			}
		}
		if strings.TrimSpace(text) == "" {
			continue // 纯工具调用意图的 assistant 消息不展示
		}
		out = append(out, item{Role: role, Text: text})
	}
	if len(out) > chatHistoryMax {
		out = out[len(out)-chatHistoryMax:]
	}
	if key == "" {
		key = "web"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"session": key, "messages": out,
	})
}

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
	// 轨迹生命周期跟随对话历史：清上下文时该会话的调用轨迹一并清除。
	removed := trace.ClearSession(key)
	msg := "会话 " + key + " 上下文已清除"
	if removed > 0 {
		msg += fmt.Sprintf("（同步清除 %d 条调用轨迹）", removed)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": msg})
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

// handleDecideApproval 审批一条待处理操作：body {"approve": true|false,
// "remember": true|false}。remember=true 为"批准并记住"——本进程内相同工具
// 与参数的后续调用免审批（权限层 Remember 语义），拒绝时忽略该字段。
func (s *Server) handleDecideApproval(ctx *vhttp.Context) error {
	if s.approver == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "审批机制未启用"})
	}
	var req struct {
		Approve  bool `json:"approve"`
		Remember bool `json:"remember"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	var id int64
	id, err := strconv.ParseInt(ctx.GinContext().Param("id"), 10, 64)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的审批 ID"})
	}
	if !s.approver.DecideWith(id, req.Approve, req.Remember) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到待审批操作（可能已超时或已处理）"})
	}
	msg := "已拒绝"
	if req.Approve {
		msg = "已批准"
		if req.Remember {
			msg += "并记住（相同调用本进程内免审批）"
		}
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": msg})
}

// handleMCPConfigGet 返回当前 MCP 服务器配置（config.yaml mcp.servers）。
func (s *Server) handleMCPConfigGet(ctx *vhttp.Context) error {
	servers := []config.McpServerConfig{}
	if s.cfg != nil {
		servers = s.cfg.Mcp.Servers
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"servers": servers})
}

// handleMCPConfigSet 保存 MCP 服务器配置：校验后写回 config.yaml（保留注释），
// 并经管理器热生效（新增的拉起、移除的停止、变更的重启，工具清单变化自动重建 Agent）。
// 请求体 {"servers":[{"name":"x","command":"npx","args":["-y","pkg"],"env":{"K":"V"}}]}。
func (s *Server) handleMCPConfigSet(ctx *vhttp.Context) error {
	if s.cfg == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	var req struct {
		Servers []config.McpServerConfig `json:"servers"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	// 校验：名称必填；command（本地 stdio）与 url（远程 HTTP）二选一；名称去重。
	seen := map[string]bool{}
	for i := range req.Servers {
		sv := &req.Servers[i]
		sv.Name = strings.TrimSpace(sv.Name)
		sv.Command = strings.TrimSpace(sv.Command)
		sv.URL = strings.TrimSpace(sv.URL)
		if sv.Name == "" {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": fmt.Sprintf("第 %d 个服务器缺少 name", i+1)})
		}
		if (sv.URL == "") == (sv.Command == "") {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": fmt.Sprintf("服务器 %s：command（本地命令）与 url（远程地址）必须二选一", sv.Name)})
		}
		if sv.URL != "" && !strings.HasPrefix(sv.URL, "http://") && !strings.HasPrefix(sv.URL, "https://") {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "服务器 " + sv.Name + " 的 url 须以 http:// 或 https:// 开头"})
		}
		key := mcp.ToolPrefix(sv.Name)
		if seen[key] {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "服务器名称重复：" + sv.Name})
		}
		seen[key] = true
	}

	// 热生效（先应用运行时，再落盘；落盘失败不影响已生效配置）。
	var applyErr error
	if s.mcp != nil {
		live := make([]mcp.ServerConfig, 0, len(req.Servers))
		for _, sv := range req.Servers {
			live = append(live, mcp.ServerConfig{
				Name: sv.Name, Command: sv.Command, Args: sv.Args, Env: sv.Env,
				URL: sv.URL, Headers: sv.Headers,
			})
		}
		applyErr = s.mcp.Apply(ctx.Context(), live)
	}
	s.cfg.Mcp.Servers = req.Servers
	msg := "MCP 配置已保存并热生效（新增拉起 / 移除停止 / 变更重启）"
	if s.mcp == nil {
		msg = "MCP 配置已保存，重启服务后生效"
	}
	if applyErr != nil {
		msg += "；部分服务器连接失败：" + applyErr.Error()
	}
	if err := config.UpdateMcpServers(s.configPath, req.Servers); err != nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{
			"message": msg + "；但写回 config.yaml 失败：" + err.Error()})
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

// handleUsage 返回 token 用量账本：按天倒序明细 + 总量。
func (s *Server) handleUsage(ctx *vhttp.Context) error {
	days, total := usage.Summary()
	if days == nil {
		days = []usage.Day{}
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"days": days, "total": total})
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
