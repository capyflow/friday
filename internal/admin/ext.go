// ext.go 是管理后台的扩展接口（M2–M5 新增）：待审批操作、上下文与调用轨迹、
// MCP 服务器状态、心跳清单编辑（网页会话的增删改查在 sessions.go）。
// 路由挂载见 admin.go 的 Start。
package admin

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/capyflow/vortexagent/llm"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/trace"
)

// chatHistoryMax 是历史回显的最大条数（老会话只展示最近一段，避免页面过长）。
const chatHistoryMax = 100

// 历史回显的文本裁剪上限：与 SSE 实时帧的口径对齐（工具摘要 160 字），
// 思考全文放宽到 4000 字——刷新后展开思考块要能看到大致完整的推理。
const (
	histThinkMax = 4000
	histTextMax  = 8000
	histToolMax  = 160
)

// histBlock 是 bot 一轮回答里的一个内容块（前端按到达顺序渲染成时间线）。
// kind：think（推理全文）| tool（工具调用，text=参数摘要、out=结果摘要）| text（正文片段）。
type histBlock struct {
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Out    string `json:"out,omitempty"`
}

// histItem 是一条回显消息：user 只有 text；bot 带 blocks（text 为正文拼接，
// 供搜索与旧前端兜底）与 turn（本轮 token 汇总，取自调用轨迹，刷新后仍在原位）。
type histItem struct {
	Role   string           `json:"role"`
	Text   string           `json:"text"`
	Blocks []histBlock      `json:"blocks,omitempty"`
	Turn   *trace.TurnUsage `json:"turn,omitempty"`
}

// turnKeyLen 是问答配对取的前缀长度（rune）。轨迹两侧都按 maxText 截断、
// 回显按 histTextMax 截断，只有前缀在两边都一定完整。
const turnKeyLen = 64

// turnKey 归一化出一次问答的配对键：去首尾空白后取前 turnKeyLen 个 rune。
func turnKey(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > turnKeyLen {
		return string(r[:turnKeyLen])
	}
	return s
}

// attachTurnUsage 把调用轨迹里的每轮用量配回对应的 bot 消息，让"本轮 · x+y tokens"
// 在刷新后仍留在它自己那一轮下面，而不是只有当前这一次对话有数字。
// 配对口径：一次问答 = 会话里一条 user 消息之后的全部 assistant 消息
// = 轨迹里同一条 Request。先按提问原文配（Input），配不上再按回答配（Answer）——
// 技能与自定义指令会改写会话里的提问文本，这时只有回答对得上。
// 同一句话问多次时从最近的一轮往前配：回显只取最后 chatHistoryMax 条，
// 页面可见的正是最近的这几轮。
func attachTurnUsage(items []histItem, reqs []trace.Request) {
	// reqs 按时间升序（sessionTurns 保证），同一个键可能对应多次问答。
	byInput, byAnswer := map[string][]int{}, map[string][]int{}
	for i := range reqs {
		if k := turnKey(reqs[i].Input); k != "" {
			byInput[k] = append(byInput[k], i)
		}
		if k := turnKey(reqs[i].Answer); k != "" {
			byAnswer[k] = append(byAnswer[k], i)
		}
	}
	used := make([]bool, len(reqs))
	pop := func(queues map[string][]int, key string) *trace.TurnUsage {
		if key == "" {
			return nil
		}
		q := queues[key]
		for i := len(q) - 1; i >= 0; i-- {
			if used[q[i]] {
				continue
			}
			used[q[i]] = true
			queues[key] = q[:i]
			return reqs[q[i]].Usage()
		}
		return nil
	}

	type turnRef struct {
		q, a string
		i    int
	}
	var refs []turnRef
	question := ""
	for i, it := range items {
		if it.Role == "user" {
			question = turnKey(it.Text)
			continue
		}
		refs = append(refs, turnRef{q: question, a: turnKey(it.Text), i: i})
	}
	for k := len(refs) - 1; k >= 0; k-- {
		r := refs[k]
		if u := pop(byInput, r.q); u != nil {
			items[r.i].Turn = u
			continue
		}
		if u := pop(byAnswer, r.a); u != nil {
			items[r.i].Turn = u
		}
	}
}

// historyItems 把会话消息投影成前端可见的对话流：一次用户提问之后的所有
// assistant/tool 轮次（含思考块与工具调用）合并成一条 bot 消息，
// 这样刷新页面后思考过程与工具调用还在。
func historyItems(msgs []llm.Message) []histItem {
	var (
		out    []histItem
		blocks []histBlock
		// openTools 保存结果尚未落定的工具调用：toolCallID → blocks 下标。
		openTools = map[string]int{}
	)

	flush := func() {
		if len(blocks) == 0 {
			return
		}
		var text strings.Builder
		for _, b := range blocks {
			if b.Kind == "text" {
				text.WriteString(b.Text)
			}
		}
		cp := make([]histBlock, len(blocks))
		copy(cp, blocks)
		out = append(out, histItem{Role: "assistant", Text: text.String(), Blocks: cp})
		blocks = nil
		openTools = map[string]int{}
	}

	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			flush()
			out = append(out, histItem{Role: string(m.Role), Text: historyText(m)})
		case llm.RoleAssistant:
			// 思考先于正文：provider 组装 Content 时把 text 排在 thinking 之前
			// （openai.go 流结束后的 append 顺序），照原样投影会把思考渲染到
			// 回答后面。这里固定按"先思考后回答"的时间线顺序输出。
			for _, c := range m.Content {
				if c.Type == llm.ContentThinking && strings.TrimSpace(c.Thinking) != "" {
					blocks = append(blocks, histBlock{
						Kind: "think", Text: logx.Truncate(c.Thinking, histThinkMax)})
				}
			}
			for _, c := range m.Content {
				switch c.Type {
				case llm.ContentText:
					if strings.TrimSpace(c.Text) != "" {
						blocks = append(blocks, histBlock{
							Kind: "text", Text: logx.Truncate(c.Text, histTextMax)})
					}
				case llm.ContentImage:
					blocks = append(blocks, histBlock{Kind: "text", Text: "[图片]"})
				}
			}
			for _, tc := range m.ToolCalls {
				openTools[tc.ID] = len(blocks)
				blocks = append(blocks, histBlock{Kind: "tool", Name: tc.Name,
					Text: agent.SummarizeArgs(tc.Arguments), Status: "ok"})
			}
		case llm.RoleTool:
			// 工具结果回填到对应调用上（失败时框架把错误写成 "工具执行出错: ..."）。
			i, ok := openTools[m.ToolCallID]
			if !ok {
				continue
			}
			result := historyText(m)
			blocks[i].Out = logx.Truncate(result, histToolMax)
			if strings.HasPrefix(result, "工具执行出错") {
				blocks[i].Status = "fail"
			}
		default:
			// system 与其他角色不进对话流。
		}
	}
	flush()
	return out
}

// historyText 拼出一条消息的全部文本内容（图片降级为占位标记）。
func historyText(m llm.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		switch c.Type {
		case llm.ContentText:
			b.WriteString(c.Text)
		case llm.ContentImage:
			b.WriteString("[图片]")
		}
	}
	return b.String()
}

// handleChatHistory 返回指定会话（?session=key，缺省 web）的可见对话历史：
// 按轮次聚合的 user/bot 消息，bot 带思考块、工具调用块与本轮 token 汇总，
// 供打开页面/刷新后回显。只读查询——不触发 LLM 调用，也不创建空会话。
func (s *Server) handleChatHistory(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"messages": []any{}})
	}
	key := strings.TrimSpace(ctx.GinContext().Query("session"))
	msgs := s.chat.Brain().Pool().History(ctx.Context(), key)
	// 系统消息（会话开始时注入的长期记忆速览）只给模型看，不往对话历史里回显
	shown := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != llm.RoleSystem {
			shown = append(shown, m)
		}
	}
	out := historyItems(shown)
	if len(out) > chatHistoryMax {
		out = out[len(out)-chatHistoryMax:]
	}
	if key == "" {
		key = "web"
	}
	attachTurnUsage(out, sessionTurns(key))
	// 被暂停的问答随历史返回（页面刷新后仍可继续）。
	body := map[string]any{"session": key, "messages": out}
	if pt := s.chat.PausedTurn(key); pt != nil {
		body["paused"] = pt
	}
	return ctx.JsonResponse(vhttp.Codes.Success, body)
}

// sessionTurns 取该会话的全部调用轨迹摘要（时间升序），供历史回显配每轮 token。
// 轨迹与对话历史同生命周期（清上下文时一并清除），所以正常状态下两边是对得上的。
func sessionTurns(key string) []trace.Request {
	all := trace.List() // 最新在前
	out := make([]trace.Request, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Session == key {
			out = append(out, all[i])
		}
	}
	return out
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
	if err := s.chat.ResetContext(key); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "清除失败: " + err.Error()})
	}
	// 轨迹生命周期跟随对话历史：清上下文时该会话的调用轨迹一并清除。
	removed := trace.ClearSession(key)
	msg := "会话 " + key + " 上下文已清除"
	if removed > 0 {
		msg += fmt.Sprintf("（同步清除 %d 条调用轨迹）", removed)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": msg})
}

// handleContextUsage GET /api/context?session=key —— 返回会话的上下文占用快照
// （活跃区估算 token、窗口、比例、是否已达压缩闸门），供对话页的上下文 chip 展示。
// 只读：不触发 LLM 调用，也不因为查一次占用就创建空会话。
func (s *Server) handleContextUsage(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, agent.ContextSnapshot{})
	}
	key := strings.TrimSpace(ctx.GinContext().Query("session"))
	return ctx.JsonResponse(vhttp.Codes.Success, s.chat.Brain().ContextSnapshot(key))
}

// handleContextCompact POST /api/context/compact —— 手动压缩一次上下文：body
// {"session":"key"} 可选，缺省压缩默认网页会话。压缩是同步的（要等一次摘要生成），
// 返回压缩前后的快照与一句结论。
func (s *Server) handleContextCompact(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用"})
	}
	var req struct {
		Session string `json:"session"`
	}
	_ = ctx.UnmarshalBody(&req)
	b := s.chat.Brain()
	before := b.ContextSnapshot(strings.TrimSpace(req.Session))
	after, err := b.CompactContext(strings.TrimSpace(req.Session))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": compactMessage(before, after),
		"before":  before,
		"after":   after,
	})
}

// compactMessage 组织压缩结果的一句话。闸门（canCompact）放行但活跃块数没超过
// 保留数时，框架的归档会静默返回——这时确实无事可做，如实说明，别报个"已压缩"。
func compactMessage(before, after agent.ContextSnapshot) string {
	archived := before.Blocks - after.Blocks
	switch {
	case after.Messages == before.Messages && archived <= 0:
		return fmt.Sprintf("没有可归档的历史（活跃 %d 块，压缩后仍保留最近 %d 块）", before.Blocks, before.KeepRecent)
	case archived > 0:
		return fmt.Sprintf("已归档 %d 块，占用 %d → %d tokens", archived, before.Used, after.Used)
	default:
		return "上下文已压缩"
	}
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
		servers = s.cfg.Snapshot().Mcp.Servers
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"servers": servers})
}

// handleMCPConfigSet 保存 MCP 服务器配置：校验后写回 config.yaml（保留注释），
// 并经管理器热生效（新增的拉起、移除的停止、变更的重启，工具清单变化自动重建 Agent）。
// 请求体 {"servers":[{"name":"x","command":"npx","args":["-y","pkg"],"env":{"K":"V"}}]}。
func (s *Server) handleMCPConfigSet(ctx *vhttp.Context) error {
	s.mcpUpdateMu.Lock()
	defer s.mcpUpdateMu.Unlock()
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
		// 鉴权方式：只认 header / oauth；OAuth 只对远程服务器有意义。
		sv.Auth = strings.ToLower(strings.TrimSpace(sv.Auth))
		if sv.Auth != "" && sv.Auth != mcp.AuthHeader && sv.Auth != mcp.AuthOAuth {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "服务器 " + sv.Name + " 的鉴权方式只支持 header 或 oauth"})
		}
		if sv.Auth == mcp.AuthOAuth && sv.URL == "" {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "服务器 " + sv.Name + "：OAuth 授权只适用于远程地址（url）"})
		}
		key := mcp.ToolPrefix(sv.Name)
		if seen[key] {
			return ctx.JsonResponse(vhttp.Codes.BadRequest,
				map[string]string{"error": "服务器名称重复：" + sv.Name})
		}
		seen[key] = true
	}

	// 保存失败不改变配置或拉起进程；文件更新与其它配置事务串行。
	if err := s.cfg.Apply(func(next *config.Config) error {
		next.Mcp.Servers = req.Servers
		return config.UpdateMcpServers(s.configPath, req.Servers)
	}); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "保存 MCP 配置失败: " + err.Error()})
	}
	// 应用回调会读取已提交快照，不在 Config 写锁内调用。
	var applyErr error
	if s.mcp != nil {
		live := make([]mcp.ServerConfig, 0, len(req.Servers))
		for _, sv := range req.Servers {
			// 字段集与 config.McpServerConfig 一致，直接类型转换（新增字段时
			// 靠编译报错提醒，避免悄悄丢掉 auth 这类配置）。
			live = append(live, mcp.ServerConfig(sv))
		}
		applyErr = s.mcp.Apply(ctx.Context(), live)
	}
	msg := "MCP 配置已保存并热生效（新增拉起 / 移除停止 / 变更重启）"
	if s.mcp == nil {
		msg = "MCP 配置已保存，重启服务后生效"
	}
	if applyErr != nil {
		msg += "；部分服务器连接失败：" + applyErr.Error()
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
	s.mcpUpdateMu.Lock()
	defer s.mcpUpdateMu.Unlock()
	if s.mcp == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "MCP 未启用"})
	}
	name := ctx.GinContext().Param("name")
	if err := s.mcp.Restart(name); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已重连 " + name})
}

// handleMCPAuthorize 为声明了 OAuth 的远程 MCP 服务器发起一次浏览器授权：
// 后台完成元数据发现与客户端注册，把授权地址返回给前端交给系统浏览器打开，
// 用户在该页面登录服务商后回调自动落到本进程，令牌入库并自动重连。
func (s *Server) handleMCPAuthorize(ctx *vhttp.Context) error {
	name := ctx.GinContext().Param("name")
	url, err := s.mcp.Authorize(ctx.Context(), name)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"url":     url,
		"message": "请在浏览器里完成 " + name + " 的登录授权，完成后自动连接",
	})
}

// handleMCPAuthStatus 查询一台服务器的授权状态（前端点「授权」后轮询这个接口）。
// url 是进行中流程的授权地址：自动打开浏览器失败或页面刷新后，前端用它做「复制链接」。
func (s *Server) handleMCPAuthStatus(ctx *vhttp.Context) error {
	name := ctx.GinContext().Param("name")
	state, message := s.mcp.AuthState(name)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"state":   state,
		"message": message,
		"url":     s.mcp.AuthURL(name),
	})
}

// handleMCPDisconnect 断开一台服务器的授权：向服务商撤销令牌并删除本地会话，
// 连接随之关闭，状态回到「待授权」。
func (s *Server) handleMCPDisconnect(ctx *vhttp.Context) error {
	s.mcpUpdateMu.Lock()
	defer s.mcpUpdateMu.Unlock()
	name := ctx.GinContext().Param("name")
	if err := s.mcp.Disconnect(ctx.Context(), name); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已断开 " + name + " 的授权"})
}

// handleGetPermissions 返回全局权限模式与相关策略（设置页展示）。
func (s *Server) handleGetPermissions(ctx *vhttp.Context) error {
	out := map[string]any{"mode": config.PermModeConfirm, "execMode": "", "mcp": "allow"}
	if s.cfg != nil {
		cfg := s.cfg.Snapshot()
		out["mode"] = cfg.Permissions.EffectiveMode()
		out["execMode"] = cfg.Tools.Exec.Mode
		if cfg.Permissions.MCP != "" {
			out["mcp"] = cfg.Permissions.MCP
		}
	}
	return ctx.JsonResponse(vhttp.Codes.Success, out)
}

// handleSetPermissions 切换全局权限模式（confirm/acceptEdits/fullAccess）：
// 经 agent.ReloadPermissions 热生效（重建权限 Checker + 更新审批队列编辑免审
// 标记）并写回 config.yaml。body {"mode": "acceptEdits"}。
func (s *Server) handleSetPermissions(ctx *vhttp.Context) error {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if s.cfg == nil || s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置或聊天服务未启用，权限模式无法热生效"})
	}
	if err := agent.ReloadPermissions(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver, req.Mode); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	mode := s.cfg.Snapshot().Permissions.EffectiveMode()
	hint := map[string]string{
		config.PermModeConfirm:     "危险操作需网页批准",
		config.PermModeAcceptEdits: "写文件/改 MCP 配置免审批，执行命令仍需批准",
		config.PermModeFullAccess:  "全部免审批（毁灭性命令黑名单仍拦截）",
	}[mode]
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{
		"mode":    mode,
		"message": "权限模式已切换为 " + mode + "并热生效（" + hint + "），已写回 config.yaml",
	})
}

// handleGetRuntime 返回内置管家的运行上限（设置页「运行上限」展示）：
// 当前生效的工具轮数上限 + 可填区间与默认值，前端据此渲染输入框提示。
func (s *Server) handleGetRuntime(ctx *vhttp.Context) error {
	out := map[string]any{
		"maxIterations":        config.DefaultAgentMaxIterations,
		"defaultMaxIterations": config.DefaultAgentMaxIterations,
		"minIterations":        config.MinAgentMaxIterations,
		"maxIterationsLimit":   config.MaxAgentMaxIterations,
	}
	if s.cfg != nil {
		out["maxIterations"] = s.cfg.Snapshot().Agent.MaxIterationsOrDefault()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, out)
}

// handleSetRuntime 保存一轮回答的工具调用轮数上限：经
// agent.ReloadAgentMaxIterations 热生效（重建 Agent + 写回 config.yaml）。
// body {"maxIterations": 60}，填 0 表示恢复默认。
func (s *Server) handleSetRuntime(ctx *vhttp.Context) error {
	var req struct {
		MaxIterations *int `json:"maxIterations"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if req.MaxIterations == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "缺少 maxIterations 字段"})
	}
	if s.cfg == nil || s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": "配置或聊天服务未启用，轮数上限无法热生效"})
	}
	n, err := agent.ReloadAgentMaxIterations(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver, *req.MaxIterations)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"maxIterations": n,
		"message":       fmt.Sprintf("单轮工具调用上限已设为 %d 轮并热生效（下一轮起），已写回 config.yaml", n),
	})
}

// handleGetPersona 返回人设提示词配置：当前生效 ID + 全部可用预设
// （内置在前、自定义在后；自定义覆盖内置时替换同 ID 文案并打标记）。
// builtin=true 表示该 ID 有内置版本（覆盖后可"恢复内置"）；
// custom=true 表示该条是自定义数据（删除不影响内置原文）。
func (s *Server) handleGetPersona(ctx *vhttp.Context) error {
	cfg := s.cfg.Snapshot()
	customByID := map[string]config.Persona{}
	for _, p := range cfg.Persona.Presets {
		customByID[p.ID] = p
	}
	builtinIDs := map[string]bool{}
	for _, p := range agent.BuiltinPersonas() {
		builtinIDs[p.ID] = true
	}
	type item struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Tagline string `json:"tagline"`
		Prompt  string `json:"prompt"`
		Builtin bool   `json:"builtin"`
		Custom  bool   `json:"custom"`
	}
	out := make([]item, 0, len(builtinIDs)+len(customByID))
	for _, p := range agent.BuiltinPersonas() {
		if cp, ok := customByID[p.ID]; ok {
			out = append(out, item{cp.ID, cp.Name, cp.Tagline, cp.Prompt, true, true})
			continue
		}
		out = append(out, item{p.ID, p.Name, p.Tagline, p.Prompt, true, false})
	}
	for _, p := range cfg.Persona.Presets {
		if builtinIDs[p.ID] {
			continue // 覆盖内置的已在上面输出
		}
		out = append(out, item{p.ID, p.Name, p.Tagline, p.Prompt, false, true})
	}
	active := cfg.Persona.Active
	if active == "" {
		active = "default"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"active": active, "presets": out,
	})
}

// handleSetPersona 切换人设/保存自定义人设（经 agent.ReloadPersona 热生效）：
// body {"active": "playful", "presets": [...]}——presets 可省略（仅切换），
// 提供则整体替换自定义预设（同 ID 覆盖内置文案，新 ID 即自定义人设）。
func (s *Server) handleSetPersona(ctx *vhttp.Context) error {
	var req struct {
		Active  string           `json:"active"`
		Presets []config.Persona `json:"presets"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "聊天服务未启用，人设无法热生效"})
	}
	if err := agent.ReloadPersona(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver,
		req.Active, req.Presets); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{
		"message": "人设已切换并热生效（对话上下文保持连续），已写回 config.yaml",
	})
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

// ---------- 模型库 ----------

// modelView 是模型库条目的对外视图：密钥不回传，只回 keySet（是否已配置）。
type modelView struct {
	Name          string   `json:"name"`
	BaseURL       string   `json:"baseURL"`
	Model         string   `json:"model"`
	KeySet        bool     `json:"keySet"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
	Inputs        []string `json:"inputs"`
}

// newModelView 组装条目视图。激活条目的密钥可来自全局扁平字段（库内条目省写、
// 沿用现状的形态），故并上 globalKeySet；输出预算同理——条目未填时激活条目显示
// 扁平值（那才是真正生效的预算），非激活条目按默认值展示，避免保存时把用户手写的
// provider.maxTokens 悄悄打成 4096。
func newModelView(m config.ModelProfile, active, globalKeySet bool, flatMaxTokens int) modelView {
	inputs := m.EffectiveInputs()
	keySet := m.APIKey != "" || m.APIKeyEnv != ""
	if active && globalKeySet {
		keySet = true
	}
	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		if active && flatMaxTokens > 0 {
			maxTokens = flatMaxTokens
		} else {
			maxTokens = config.DefaultMaxTokens
		}
	}
	return modelView{
		Name: m.Name, BaseURL: m.BaseURL, Model: m.Model,
		KeySet: keySet, ContextWindow: m.ContextWindow, MaxTokens: maxTokens, Inputs: inputs,
	}
}

// effectiveProviderModels 返回模型库对外展示的条目与激活名。首次启动种子里的
// 示例占位（api.example.com / 请填模型名）不算已配置：单条占位库与扁平占位都
// 按空库返回，前端据此显示「还没有配置模型」而不是一条假模型。
func effectiveProviderModels(p config.ProviderConfig) (string, []config.ModelProfile) {
	if len(p.Models) == 1 && p.Models[0].IsPlaceholder() {
		return "", nil
	}
	if len(p.Models) > 0 {
		return p.Active, p.Models
	}
	flat := config.ModelProfile{
		Name: p.Model, BaseURL: p.BaseURL, Model: p.Model,
		ContextWindow: p.ContextWindow, MaxTokens: p.MaxTokens,
	}
	if strings.TrimSpace(flat.BaseURL) == "" || strings.TrimSpace(flat.Model) == "" || flat.IsPlaceholder() {
		return "", nil
	}
	return p.Model, []config.ModelProfile{flat}
}

// handleProviderModelsGet 返回模型库与当前激活条目。未建库时用扁平字段合成
// 一条（name = model 名），前端无需区分两种形态；密钥不回传，只回 keySet。
// 初始配置的示例占位按空库返回（见 effectiveProviderModels）。
func (s *Server) handleProviderModelsGet(ctx *vhttp.Context) error {
	cfg := s.cfg.Snapshot()
	active, models := effectiveProviderModels(cfg.Provider)
	globalKeySet := cfg.ProviderAPIKey() != ""
	views := make([]modelView, 0, len(models))
	for _, m := range models {
		views = append(views, newModelView(m, m.Name == active, globalKeySet, cfg.Provider.MaxTokens))
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"active": active, "models": views,
		// 全局显式覆盖（context.windowTokens，0 = 未设置，UI 提示其优先级最高）。
		"globalWindowTokens": cfg.Context.WindowTokens,
	})
}

// handleProviderModelsSet 保存模型库并激活指定条目（全量式保存，同 MCP 配置）：
// 校验与密钥继承在 ReloadProviderModels 内做，按变化范围决定是否热重建。
func (s *Server) handleProviderModelsSet(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	var req struct {
		Active string                `json:"active"`
		Models []config.ModelProfile `json:"models"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if len(req.Models) == 0 {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "模型库不能为空，至少保留一条"})
	}
	if err := agent.ReloadProviderModels(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver, req.Active, req.Models); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "模型库已保存并热生效，已写回 config.yaml"})
}

// handleProviderActivate 切换激活模型（聊天页快捷切换）：复用模型库保存内核，
// 库保持原样只改 active，密钥按条目自带（或沿用现状）。
func (s *Server) handleProviderActivate(ctx *vhttp.Context) error {
	if s.chat == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	name := strings.TrimSpace(req.Name)
	cfg := s.cfg.Snapshot()
	if _, ok := cfg.Provider.ActiveModel(); !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "尚未配置模型库，先在「设置 → 模型服务」添加"})
	}
	if cfg.Provider.Active == name {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "当前已在使用 " + name})
	}
	if err := agent.ReloadProviderModels(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver,
		name, append([]config.ModelProfile{}, cfg.Provider.Models...)); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已切换到 " + name})
}
