// acp.go 是智能体对话空间的管理后台接口：thread CRUD、SSE 对话流、停止、
// 配置读写与 agent 握手诊断。SSE 帧协议与 /api/chat 完全同形
// （delta/thinking/step/done，另有 status 生命周期占位帧），前端时间线渲染组件两端共用。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

// handleAcpConfigGet 返回智能体配置（已归一化的默认值一并回显）。
func (s *Server) handleAcpConfigGet(ctx *vhttp.Context) error {
	cfg := config.AcpConfig{}
	if s.cfg != nil {
		cfg = s.cfg.Snapshot().Acp.Normalized()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"acp": cfg})
}

// handleAcpConfigSet 保存智能体配置：校验 → 写回 config.yaml → Manager 热生效
// （配置变化的 agent 回收进程，上下文由下次打开时 session/load 尝试恢复）。
func (s *Server) handleAcpConfigSet(ctx *vhttp.Context) error {
	s.acpUpdateMu.Lock()
	defer s.acpUpdateMu.Unlock()
	if s.cfg == nil || s.configPath == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	var req struct {
		Acp config.AcpConfig `json:"acp"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	next := req.Acp.Normalized()
	if err := next.Validate(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if err := s.cfg.Apply(func(n *config.Config) error {
		n.Acp = next
		return config.UpdateAcp(s.configPath, next)
	}); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	s.acp.Reload(next)
	enabled := "关闭"
	if next.Enabled {
		enabled = "开启"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "智能体配置已保存（功能" + enabled + "，" + fmt.Sprint(len(next.Agents)) + " 个 agent），并已写回 config.yaml",
		"acp":     next,
	})
}

// handleAcpThreads 返回 thread 列表（元数据，不含消息）与当前配置。
func (s *Server) handleAcpThreads(ctx *vhttp.Context) error {
	cfg := config.AcpConfig{}
	if s.cfg != nil {
		cfg = s.cfg.Snapshot().Acp.Normalized()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"threads": s.acp.List(),
		"acp":     cfg,
	})
}

// handleAcpThreadGet 返回单个 thread（含投影消息）。
func (s *Server) handleAcpThreadGet(ctx *vhttp.Context) error {
	th, ok := s.acp.Get(ctx.GinContext().Param("id"))
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到该对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"thread": th})
}

// handleAcpThreadCreate 新建 thread：{"agent":"codex","workspace":"~/proj","title":"可选"}。
// agent 与工作目录在此刻绑定，之后不可切换（拍板 D1）。
func (s *Server) handleAcpThreadCreate(ctx *vhttp.Context) error {
	var req struct {
		Agent     string `json:"agent"`
		Workspace string `json:"workspace"`
		Title     string `json:"title"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	th, err := s.acp.Create(strings.TrimSpace(req.Agent), req.Workspace, req.Title)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"thread": th})
}

// handleAcpThreadDelete 删除 thread（连同投影记录与存活进程）。
func (s *Server) handleAcpThreadDelete(ctx *vhttp.Context) error {
	if !s.acp.Delete(ctx.GinContext().Param("id")) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到该对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除该对话"})
}

// handleAcpStop 取消进行中的一轮（显式 session/cancel；上下文保留，可直接续聊）。
func (s *Server) handleAcpStop(ctx *vhttp.Context) error {
	if !s.acp.Stop(ctx.GinContext().Param("id")) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有进行中的对话"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已发送停止指令"})
}

// handleAcpCatalog 返回可安装 agent 目录与本机安装状态（设置页「安装 agent」用）。
func (s *Server) handleAcpCatalog(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"catalog": s.acp.Catalog()})
}

// handleAcpInstall 执行目录中指定 agent 的官方安装命令（同步，最长 5 分钟）：
// {"id":"opencode"}，返回输出尾部。命令为内置目录固定串，不经任何外部输入拼接。
func (s *Server) handleAcpInstall(ctx *vhttp.Context) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	out, err := s.acp.Install(ctx.Context(), req.ID)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error(), "output": out})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"message": "安装完成", "output": out})
}

// handleAcpTest 对指定 agent 做握手诊断（设置页「测试」按钮）。
func (s *Server) handleAcpTest(ctx *vhttp.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := s.acp.Test(strings.TrimSpace(req.Name)); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "握手成功：agent 可正常拉起并响应 initialize"})
}

// handleAcpChat 处理一次智能体对话：POST /api/acp/threads/:id/chat，
// 请求体 {"message":"...", "attachments":["<上传id>", ...]}，响应为 SSE 流，
// 帧协议与 /api/chat 一致（另有 {"status":"..."} 生命周期占位帧）。
func (s *Server) handleAcpChat(ctx *vhttp.Context) error {
	var req struct {
		Message     string   `json:"message"`
		Attachments []string `json:"attachments"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil ||
		(strings.TrimSpace(req.Message) == "" && len(req.Attachments) == 0) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": `请求体须为 {"message": "...", "attachments": [...]}`})
	}
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用（图片上传链路不可用）"})
	}
	threadID := ctx.GinContext().Param("id")

	// 附件 → 图片内容（复用聊天页的上传链路与目录）。
	var imgs []acp.Image
	for _, id := range req.Attachments {
		path, ok := s.chat.ResolveUpload(id)
		if !ok {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的附件 ID: " + id})
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "附件不存在或已过期"})
		}
		imgs = append(imgs, acp.Image{MIME: detectImageMIME(data), Data: data})
	}

	gin := ctx.GinContext()
	gin.Header("Content-Type", "text/event-stream; charset=utf-8")
	gin.Header("Cache-Control", "no-cache")
	gin.Header("X-Accel-Buffering", "no")
	gin.Writer.WriteHeader(http.StatusOK)
	flusher, ok := gin.Writer.(http.Flusher)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "连接不支持流式响应"})
	}
	var wmu sync.Mutex
	write := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	rctx, cancel := context.WithCancel(logx.WithID(ctx.Context(), logx.NextID()))
	defer cancel()
	question := strings.TrimSpace(req.Message)
	trace.Begin(logx.IDFrom(rctx), "agent", "acp:"+threadID, question)
	defer trace.End(logx.IDFrom(rctx), "")

	res, err := s.acp.Send(rctx, threadID, question, imgs, acp.TurnHooks{
		OnText: func(t string) {
			if !write(map[string]string{"delta": t}) {
				cancel() // 客户端已断开，中止对话
			}
		},
		OnThought: func(t string) {
			if !write(map[string]string{"thinking": t}) {
				cancel()
			}
		},
		OnStep: func(st agent.Step) {
			if !write(map[string]any{"step": st}) {
				cancel()
			}
		},
		OnStatus: func(msg string) {
			write(map[string]string{"status": msg})
		},
	})
	if err != nil {
		logx.Printf(rctx, "[智能体] 对话失败: %v", err)
		trace.Record(rctx, "acp", "失败: %v", err)
		write(map[string]any{"done": true, "error": err.Error()})
		return nil
	}
	trace.Record(rctx, "acp", "stopReason=%s，token in=%d out=%d", res.StopReason, res.In, res.Out)
	turn := map[string]any{"in": res.In, "out": res.Out, "ms": res.Ms}
	if res.StopReason != "" && res.StopReason != "end_turn" {
		turn["stopped"] = res.StopReason
	}
	write(map[string]any{"done": true, "turn": turn})
	return nil
}

// detectImageMIME 嗅探图片类型（与聊天页上传白名单一致：png/jpeg/webp）。
func detectImageMIME(data []byte) string {
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff" {
		return "image/jpeg"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "application/octet-stream"
}
