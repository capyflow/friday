// Package agent 把 vortexagent 框架组装成 Friday 个人管家：创建 LLM Provider、
// 注册工具、配置生命周期钩子（日志/拦截）、注入面向生活管家场景的中文系统提示词。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/ctxengine"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/skill"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
	tools_pkg "github.com/capyflow/friday/internal/tools"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// Build 创建并返回配置好的 Agent。pool 为会话池：轨迹钩子在每轮 LLM 调用时
// 对"当前请求的会话"做输入快照，从而记录下发给模型的完整上下文。
// approver 为审批队列：构建全局权限 Checker（规则翻译 + ApprovalUI 桥接），
// nil 时按 off 语义兜底（exec_command 不注册、其余工具直通）。
// engine 为上下文引擎（ContextManager.Rebuild 产出），nil 时退化为
// 「每轮发送完整历史」的旧行为。调用方在 Build 后经 Brain 初始化会话
// （池按 key 惰性创建）。
func Build(cfg *config.Config, registry *vxagent.Registry, pool *SessionPool, approver *approval.Queue, engine *ctxengine.Engine) (*vxagent.Agent, error) {
	cfg = cfg.Snapshot()
	provider, err := newProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 LLM provider: %w", err)
	}
	if approver == nil {
		approver = approval.New(string(approval.ModeOff), nil, 0)
	}
	perms, err := NewPermissionChecker(cfg, approver)
	if err != nil {
		return nil, fmt.Errorf("组装权限层: %w", err)
	}

	prompt := systemPrompt(cfg)
	ag := vxagent.New(vxagent.Options{
		Provider:      provider,
		Registry:      registry,
		SystemPrompt:  prompt,
		Hooks:         buildHooks(pool, prompt),
		Permissions:   perms,
		ContextEngine: engine,
		// 一轮回答允许的工具调用轮数（agent.maxIterations，默认 100）；
		// 不填则用框架缺省 10 轮——真实活儿半路就断。计划/目标轮另有自己的
		// 按次上限（WithMaxIterations），优先级高于这里。
		MaxIterations: cfg.Agent.MaxIterationsOrDefault(),
	})
	return ag, nil
}

// newProvider 按配置构建 LLM provider（Build 与上下文引擎摘要器共用）。
// 密钥允许为空（未配置不影响启动，管理后台/待办/任务照常可用）：
// LLM 请求在配置密钥前会失败并提示去「设置」页配置。
func newProvider(cfg *config.Config) (llm.Provider, error) {
	cfg = cfg.Snapshot()
	apiKey := cfg.ProviderAPIKey()
	temp := cfg.Provider.Temperature
	protocol := cfg.Provider.EffectiveProtocol()
	if protocol == config.ProtocolAnthropic {
		// Anthropic Messages 协议：官方用 x-api-key，而 MiniMax / 智谱等兼容
		// 网关用 Authorization: Bearer。底层 provider 只发 x-api-key，这里用
		// 自定义 transport 给非官方主机补一个 Bearer 头，两边都能用。
		return llm.NewAnthropicProvider(apiKey,
			llm.WithBaseURL(cfg.Provider.BaseURL),
			llm.WithModel(cfg.Provider.Model),
			llm.WithMaxTokens(cfg.Provider.MaxTokens),
			llm.WithTemperature(temp),
			llm.WithHTTPClient(anthropicHTTPClient(apiKey)),
		), nil
	}
	return llm.NewProvider(llm.ProviderConfig{
		Name:        protocol,
		APIKey:      apiKey,
		BaseURL:     cfg.Provider.BaseURL,
		Model:       cfg.Provider.Model,
		MaxTokens:   cfg.Provider.MaxTokens,
		Temperature: &temp,
		StreamUsage: cfg.Provider.StreamUsage,
	})
}

// anthropicOfficialHost 是 Anthropic 官方 API 主机：只认 x-api-key，不需要
// 额外的 Authorization（同时带上可能触发鉴权冲突）。
const anthropicOfficialHost = "api.anthropic.com"

// anthropicHTTPClient 返回给 Anthropic provider 用的 HTTP 客户端：对第三方
// Anthropic 兼容网关追加 Authorization: Bearer <key>，官方主机保持原样。
func anthropicHTTPClient(apiKey string) *http.Client {
	if apiKey == "" {
		return http.DefaultClient
	}
	return &http.Client{Transport: bearerTransport{apiKey: apiKey, base: http.DefaultTransport}}
}

// bearerTransport 在请求发往非官方 Anthropic 主机时补 Bearer 鉴权头。
type bearerTransport struct {
	apiKey string
	base   http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && !strings.EqualFold(req.URL.Hostname(), anthropicOfficialHost) {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// buildHooks 配置生命周期钩子：按请求 ID 记录完整调用链——LLM 每轮调用（轮次/
// token/耗时）、模型决定调用的工具、工具执行结果、最终回答与错误。
// 请求 ID 从 ctx 读取（入口处由 logx.WithID 注入，见 logx 包）。
// 输入快照取自 ctx 会话 key 对应的会话（多会话各自快照，互不混淆）。
// 除打日志外同步写入内存轨迹库（internal/trace），供管理后台"调用轨迹"页面查看。
func buildHooks(pool *SessionPool, sysPrompt string) *vxagent.Hooks {
	return &vxagent.Hooks{
		OnLLMCall: func(ctx context.Context, info vxagent.LLMCallInfo) {
			logx.Printf(ctx, "[llm] round=%d model=%s tokens=%d+%d 耗时=%s err=%v",
				info.Round, info.Model, info.Usage.InputTokens, info.Usage.OutputTokens,
				info.Duration, info.Err)
			usage.Record(usage.Call{
				Model:      info.Model,
				Session:    SessionKeyFrom(ctx),
				Origin:     OriginFrom(ctx),
				Run:        RunFrom(ctx),
				In:         int64(info.Usage.InputTokens),
				Out:        int64(info.Usage.OutputTokens),
				CacheRead:  int64(info.Usage.CacheReadTokens),
				CacheWrite: int64(info.Usage.CacheWriteTokens),
			})
			// 思考过程事件：每轮 LLM 调用一条，只报耗时。token 不在这里摊——
			// 它是整轮问答的量，由 SSE 的 done 帧一次性汇总给前端（chat.go 的
			// turn 字段），挂在每一行思考上既读不出信息量，也把时间线染成一串数字。
			roundDetail := fmt.Sprintf("耗时 %.1fs", info.Duration.Seconds())
			if info.Err != nil {
				roundDetail += " · " + info.Err.Error()
			}
			PublishStep(ctx, Step{Kind: "round", Detail: roundDetail})
			// 此刻会话内容 = 本轮发给模型的完整输入（助手消息在调用返回后才入会话）。
			// 第 1 轮额外带上系统提示词，凑齐真实上下文全貌。
			msgs := msgsToTrace(pool.Get(ctx, SessionKeyFrom(ctx)).Messages())
			if info.Round == 1 {
				msgs = append([]traceMsg{{Role: "system", Text: sysPrompt}}, msgs...)
			}
			b, _ := json.Marshal(msgs)
			trace.RecordLLMCall(ctx, info.Round, string(b),
				int64(info.Usage.InputTokens), int64(info.Usage.OutputTokens),
				info.Duration.Milliseconds())
		},
		OnMessage: func(ctx context.Context, msg llm.Message) {
			// 模型本轮原始输出（文本 + 工具调用意图）先入轨迹，随后才是工具分发。
			trace.RecordLLMMessage(ctx, dumpMessage(msg))
			for _, tc := range msg.ToolCalls {
				logx.Printf(ctx, "[llm] 请求工具: %s %v", tc.Name, tc.Arguments)
				trace.Record(ctx, "tool_req", "模型请求调用工具 %s，参数 %v", tc.Name, tc.Arguments)
			}
			if len(msg.ToolCalls) > 0 {
				if text := messageText(msg); strings.TrimSpace(text) != "" {
					logx.Printf(ctx, "[llm] 附带文本: %s", logx.Truncate(text, 120))
				}
			}
		},
		OnBeforeToolCall: func(ctx context.Context, name string, args map[string]any) error {
			logx.Printf(ctx, "[tool] -> %s %v", name, args)
			PublishStep(ctx, Step{Kind: "tool", Name: name, Status: "run",
				Detail: SummarizeArgs(args)})
			return nil
		},
		OnAfterToolCall: func(ctx context.Context, name string, _ map[string]any, result string, err error) {
			if err != nil {
				logx.Printf(ctx, "[tool] <- %s 失败: %v", name, err)
				trace.Record(ctx, "tool", "工具 %s 执行失败：%v", name, err)
				PublishStep(ctx, Step{Kind: "tool", Name: name, Status: "fail",
					Detail: logx.Truncate(err.Error(), 160)})
				return
			}
			logx.Printf(ctx, "[tool] <- %s: %s", name, logx.Truncate(result, 120))
			trace.Record(ctx, "tool", "工具 %s 返回：%s", name, logx.Truncate(result, 500))
			PublishStep(ctx, Step{Kind: "tool", Name: name, Status: "ok",
				Detail: logx.Truncate(result, 160)})
		},
		OnFinish: func(ctx context.Context, answer string) {
			logx.Printf(ctx, "[agent] 回答: %s", logx.Truncate(answer, 200))
			trace.Record(ctx, "answer", "%s", answer)
		},
		OnError: func(ctx context.Context, err error) {
			logx.Printf(ctx, "[agent] 错误: %v", err)
			trace.Record(ctx, "error", "%v", err)
		},
	}
}

// SummarizeArgs 把工具参数压成一行摘要（思考过程展示用：SSE 步骤与历史回显共用同一口径）。
func SummarizeArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok && cmd != "" {
		return logx.Truncate(cmd, 160)
	}
	if path, ok := args["path"].(string); ok && path != "" {
		return logx.Truncate(path, 160)
	}
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return logx.Truncate(string(b), 160)
}

// traceMsg 是轨迹里一条消息的可序列化投影。
type traceMsg struct {
	Role       string      `json:"role"`
	Text       string      `json:"text,omitempty"`
	ToolCalls  []traceTool `json:"toolCalls,omitempty"`
	ToolCallID string      `json:"toolCallId,omitempty"`
}

type traceTool struct {
	Name string         `json:"name"`
	Args map[string]any `json:"arguments,omitempty"`
}

// msgsToTrace 把会话消息投影成可序列化结构。图片内容降级为占位标记，
// 避免 base64 数据撑爆轨迹。
func msgsToTrace(msgs []llm.Message) []traceMsg {
	out := make([]traceMsg, 0, len(msgs))
	for _, m := range msgs {
		text := messageText(m)
		for _, c := range m.Content {
			if c.Type == llm.ContentImage {
				text += "[图片 " + c.MIME + "]"
			}
		}
		tm := traceMsg{Role: string(m.Role), Text: text, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			tm.ToolCalls = append(tm.ToolCalls, traceTool{Name: tc.Name, Args: tc.Arguments})
		}
		out = append(out, tm)
	}
	return out
}

// dumpMessage 把模型单条输出消息序列化成 JSON。
func dumpMessage(m llm.Message) string {
	b, _ := json.Marshal(msgsToTrace([]llm.Message{m})[0])
	return string(b)
}

// messageText 拼接消息中的全部文本内容。
func messageText(m llm.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == llm.ContentText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Deps 汇总 RegisterTools 需要的管家能力依赖。
type Deps struct {
	Engine     *task.Engine      // 任务排程器（单次/多次/循环/智能任务）
	Todos      *todo.Store       // 待办清单仓库
	Memory     *memory.WikiStore // 长期记忆 wiki 仓库（主题页面）
	Ctx        *ContextManager   // 上下文引擎管理器（注册检索工具用，可为 nil）
	Speaker    output.Speaker    // 播报输出（硬件输出开启时使用）
	Displayer  output.Displayer  // 屏幕输出（硬件输出开启时使用）
	Player     audio.Player      // 音频播放器（硬件输出开启时使用）
	Config     *config.Config    // 运行配置（文件白名单、硬件开关、天气默认城市等）
	Approver   *approval.Queue   // 危险操作审批队列（权限层 ApprovalUI 桥接与 write/edit 用，可为 nil）
	Mcp        *mcp.Manager      // MCP 服务器管理器（mcp_edit 工具用，可为 nil）
	Skills     *skill.Store      // 技能仓库（read_skill 按需取正文用，可为 nil）
	ConfigPath string            // config.yaml 路径（mcp_edit 写回用）
	// Search 返回联网搜索的当前供应商与密钥（设置页保存即生效，工具每次调用读取；
	// nil 时按默认 bing 免密钥处理）。
	Search func() (provider, apiKey string)
	// Delegate 是子任务派发工具（delegate_task）。可为 nil：没接上时该工具不注册，
	// 模型看不见它，也就不会去调一个调不通的工具。
	Delegate *DelegateTool
}

// RegisterTools 把管家全部业务工具注册进 Registry。
// speak_now / display_now / play_audio / play_effect 属于硬件输出能力，
// 仅在 config hardware.enabled=true 时注册（Friday 当前与硬件分离，默认关闭）；
// approver 支撑 write_file/edit_file 的审批机制（confirm 模式）与 exec_command
// 的全局权限层（见 permission.go），nil 时 exec 按 off 禁用、文件工具直行。
func RegisterTools(r *vxagent.Registry, d Deps) error {
	fileTools := tools_pkg.FileTools{AllowDirs: d.Config.File.AllowDirs}
	var approver *approval.Queue
	if d.Approver != nil {
		approver = d.Approver
	} else {
		approver = approval.New(string(approval.ModeOff), nil, 0) // 兜底：无审批器时禁用 exec，文件工具直行
	}
	tools := []vxagent.Tool{
		&tools_pkg.ScheduleTaskTool{Engine: d.Engine},
		&tools_pkg.UpdateTaskTool{Engine: d.Engine},
		&tools_pkg.PauseTaskTool{Engine: d.Engine},
		&tools_pkg.ResumeTaskTool{Engine: d.Engine},
		&tools_pkg.ListTasksTool{Engine: d.Engine},
		&tools_pkg.CancelTaskTool{Engine: d.Engine},
		&tools_pkg.AddTodoTool{Store: d.Todos},
		&tools_pkg.UpdateTodoTool{Store: d.Todos},
		&tools_pkg.ListTodosTool{Store: d.Todos},
		&tools_pkg.CompleteTodoTool{Store: d.Todos},
		&tools_pkg.DeleteTodoTool{Store: d.Todos},
		&tools_pkg.AddNoteTool{Store: d.Todos},
		&tools_pkg.ListNotesTool{Store: d.Todos},
		&tools_pkg.UpdateNoteTool{Store: d.Todos},
		&tools_pkg.DeleteNoteTool{Store: d.Todos},
		&tools_pkg.PinEntryTool{Store: d.Todos},
		&tools_pkg.WeatherTool{DefaultCity: d.Config.Weather.DefaultCity},
		&tools_pkg.WebSearchTool{SearchFn: d.Search},
		&tools_pkg.WebFetchTool{},
		&tools_pkg.RememberTool{Memory: d.Memory},
		&tools_pkg.RecallTool{Memory: d.Memory},
		&tools_pkg.ForgetTool{Memory: d.Memory},
		&tools_pkg.GetTimeTool{},
		&tools_pkg.ReadFileTool{FileTools: fileTools},
		&tools_pkg.ListFilesTool{FileTools: fileTools},
		&tools_pkg.WriteFileTool{FileTools: fileTools, Approver: approver},
		&tools_pkg.EditFileTool{FileTools: fileTools, Approver: approver},
		// 编码面（内置 agent 自己就能改代码，不依赖外部 coding agent）：
		// 检索（search_code/find_files）、只读 git 三件套、批量精确编辑（edit_files）。
		&tools_pkg.SearchCodeTool{FileTools: fileTools},
		&tools_pkg.FindFilesTool{FileTools: fileTools},
		&tools_pkg.GitStatusTool{FileTools: fileTools},
		&tools_pkg.GitDiffTool{FileTools: fileTools},
		&tools_pkg.GitLogTool{FileTools: fileTools},
		&tools_pkg.EditFilesTool{FileTools: fileTools, Approver: approver},
		// 文件生命周期：改名/移动（move_file）与删除（delete_file，不硬删，移入回收站可恢复）。
		&tools_pkg.MoveFileTool{FileTools: fileTools, Approver: approver},
		&tools_pkg.DeleteFileTool{FileTools: fileTools, Approver: approver},
		&tools_pkg.McpEditTool{Manager: d.Mcp, Config: d.Config, ConfigPath: d.ConfigPath, Approver: approver},
		// 技能正文超过注入预算时只给概览，模型靠它取全文与 references/。
		&tools_pkg.ReadSkillTool{Store: d.Skills},
	}
	if d.Config.Hardware.Enabled {
		tools = append(tools,
			&tools_pkg.SpeakNowTool{Speaker: d.Speaker},
			&tools_pkg.DisplayNowTool{Displayer: d.Displayer},
			&tools_pkg.PlayAudioTool{Player: d.Player},
			&tools_pkg.PlayEffectTool{Player: d.Player})
	}
	// 子任务派发（delegate_task）：接线在 Brain 建好之后才补，这里只是把工具挂上。
	if d.Delegate != nil {
		tools = append(tools, d.Delegate)
	}
	for _, t := range tools {
		if err := r.Add(t); err != nil {
			return err
		}
	}
	// exec_command（框架工具 + 默认超时包装）：off 模式不注册——模型不可见即整体禁用。
	// 权限规则（白名单/毁灭性黑名单）由 Build 里的权限 Checker 承担，见 permission.go。
	if d.Config.Tools.Exec.Mode != string(approval.ModeOff) {
		if err := r.Add(NewExecCommandTool(d.Config.Tools.Exec.TimeoutSeconds(), scratchDir())); err != nil {
			return err
		}
	}
	// 上下文检索工具（search_history / expand_chunk）：引擎启用时注册，供模型
	// 按需回看已归档的早期对话——Ask 时框架会自动把检索指引追加到系统提示词。
	// 每次重建先经 ContextManager.Rebuild 再走这里，Current 拿到的是新引擎。
	if d.Ctx != nil {
		if eng := d.Ctx.Current(); eng != nil {
			for _, t := range ctxengine.NewTools(eng) {
				if err := r.Add(t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// systemPrompt 生成管家系统提示词：身份定位 + 人设段（persona.active 选择，
// 见 persona.go）+ 人文关怀与工具使用规则（所有人设共用）。
// 硬件输出工具仅在 hardware.enabled 时写入提示词（与 RegisterTools 的注册保持一致）。
func systemPrompt(cfg *config.Config) string {
	if cfg.SystemPrompt != "" {
		return cfg.SystemPrompt
	}
	var b strings.Builder
	b.WriteString(personaIdentity)
	b.WriteString("\n\n【性格与语气】\n")
	b.WriteString(resolvePersona(cfg).Prompt)
	b.WriteString("\n\n")
	b.WriteString(personaCare)
	b.WriteString("\n")
	b.WriteString(`
	【闪念：笔记与待办】（网页"闪念"页 = 笔记和待办的统一清单）
1. 随手想法、灵感、资料片段，用户说"记一下/记个笔记/存起来"：调用 add_note，文本里可用 #标签 归类（如 "#工作 周报思路"）。明确要做的事（有完成动作、可能有截止时间）用 add_todo，不要记成笔记。
2. 用户问"我之前记过什么/那条笔记写了啥"：调用 list_notes（q 关键词 / tag 标签过滤）检索；修改用 update_note，删除用 delete_note；重要的内容可 pin_entry 置顶。
3. 待办规则：交代要做的事 add_todo（相对日期先 get_time，截止用 dueAt，提醒用 remindAt/in；截止时间本身不自动提醒）；问待办 list_todos；完成 complete_todo、不用了 delete_todo、修改 update_todo（先列出拿 ID，完整替换保留未修改字段）。完成/删除/修改待办会自动取消或调整关联的未来提醒，禁止另建独立 schedule_task 造成重复。
4. 引用清单条目时带上 ID（如 #t3f…、#n8a…），方便用户在网页闪念页对上；pin_entry 对笔记和待办通用。

【定时任务】
1. 单次提醒（"10分钟后提醒我敷面膜"）：调用 schedule_task，in 填相对时长（如 10分钟/2小时/1天/90s），text 填内容。
2. 具体时刻提醒（"4点11分提醒我""明天早上8点叫我起床"）：必须先调用 get_time 获取当前时间，然后调用 schedule_task 时必须用 when 填 RFC3339 绝对时间（含时区，如 2026-09-05T16:11:00+08:00），禁止用 in 表达具体时刻。
3. 有限多次（"每10分钟提醒我一次，共3次"）：调用 schedule_task，in 填间隔（10分钟），times 填次数（3）。
4. 无限循环（"每天早上8点提醒我喝水""每周一三五9点播报天气"）：调用 schedule_task，every 填自然语言（"每天早上8点""每周一三五9:30"）或 cron 表达式。
5. 智能任务（"每天早上8点看看我的待办和天气，给我一份晨报"这类需要自主判断的任务）：调用 schedule_task，agent=true，text 填完整的任务指令，到点后由你自主决策执行。
6. 查看/取消任务：调用 list_tasks / cancel_task（先列出拿 ID 再取消，单次/多次/循环/智能通用）。
7. 查询当前时间调用 get_time。
8. 修改未来任务用 update_task，先 list_tasks 获取现有定义和 ID；暂停/恢复用 pause_task/resume_task，只影响未来触发，已开始的执行保留记录。

【天气查询】
1. 用户问天气、要不要带伞、穿什么：调用 get_weather，city 填城市名；问"未来几天"时把预报转述清楚。
2. 用户没指明城市：配置了默认城市就直接查并说明；没配置就先问用户所在城市，用户回答后可用 remember 记入"用户偏好"页（如"用户所在城市是上海"），下次直接查。

【MCP 扩展工具】（mcp_edit：接入/管理外部 MCP 服务器，变更热生效无需重启）
1. 用户发来 MCP 配置或要求接入某个外部工具/数据源（"帮我接入这个 mcp""把 blinko 配置上"）：调用 mcp_edit，action=add；远程服务器填 url/headers（鉴权令牌放 headers，如 {"Authorization":"Bearer ..."}），本地命令服务器填 command/args/env。
2. 用户问已接入了哪些 MCP/外部工具：action=list 查看全部（含连接状态与工具数）；action=get 加 name 看单台配置与工具清单。
3. 修改用 action=update（只传要改的字段，其余保留）；不再使用用 action=remove；连接异常时先 list 看状态，配置没问题时 action=restart 重连。
4. 涉及变更（add/update/remove）在 confirm 审批模式下需用户在网页批准；凭据类信息不写入长期记忆。
`)
	if cfg.Hardware.Enabled {
		b.WriteString(`
【即时输出（已接入硬件）】
- 用户要求立即播报/显示：调用 speak_now（语音播报）和 display_now（屏幕显示）。
- 用户想听声音：播放本地 wav 用 play_audio；要提示音用 play_effect（ding/dingdong/alert）。
`)
	}
	b.WriteString(`
【文件与命令】
1. 读文件用 read_file（可分段续读），列目录用 list_files；写文件用 write_file（原文件自动备份为 .bak）；精确修改用 edit_file（old 必须唯一匹配，防止误改）。
2. write_file/edit_file/exec_command 属于受控操作：confirm 审批模式下需用户在管理后台批准后才会执行，请把要做什么说清楚；收到"用户未批准"或"被权限系统拒绝"时不要反复重试同一操作。
3. 执行 shell 命令用 exec_command（默认 30 秒超时，可通过 timeout 参数最长调到 10 分钟）：只用于轻量、安全的命令，rm -rf 等毁灭性命令会被强制拦截。命令默认在本次会话的工作目录里执行（工作模式下就是那个项目目录），要用别处传 cwd。
4. 在项目目录里找代码用 search_code（按内容搜，正则或字面量）与 find_files（按文件名 glob 找）；看仓库现状用 git_status / git_diff / git_log（只读，不会改动仓库）；一次改多处或多个文件用 edit_files（old 须唯一匹配，整批要么全改要么不改）。
5. 改名/移动文件用 move_file（目标已存在默认拒绝，覆盖传 overwrite=true，旧目标会备份成 .bak）；删文件用 delete_file（不硬删，移入回收站可恢复；目录要先用 move_file 移走）。
6. 活儿能拆就拆：互相独立的调查（摸清某块代码、同时查几处）用 delegate_task 派子任务并行去做，指令要写自足（子任务看不到主对话），自己只收结论；改文件仍然自己动手。
7. 多步的活儿（要改好几处、每处都得验证）用 create_plan 先列步骤再按步执行：每步一条独立轨迹，中途断了能接上，进度在「自动化 → 计划」页看得见、也随时能停。步骤之间不要一次说完。
8. 写脚本、跑中间产物一律走系统临时目录（macOS/Linux 是 /tmp，Windows 是 %TEMP%），不要写进数据目录、工作目录或用户家目录——那些地方是长期资产，脚本和中间产物留在里面会越积越多：
   - 需要写脚本时，先在临时目录下造一个本次专用的子目录（macOS/Linux 用 mktemp -d，如 /tmp/tmp.XXXX；Windows 在 %TEMP% 下新建一个目录），脚本放进去并用绝对路径引用；不要在数据目录或工作目录根下直接建 .py/.sh 文件。
   - 脚本跑完自己删掉（连同那个临时子目录一起 rm），别把垃圾留在那儿。
   - 生成的产物（报告、导出、转换结果、中间数据）用户没指定去处时也放临时目录，并把完整路径告诉用户；临时目录里的旧文件会被系统定期清掉，用户要长期留的，就按他指定的位置另存一份。
   - 只有用户明确说了放在哪里，或产物本身就是给他长期使用的东西（改他的项目文件、交付给他保留的资料），才写到临时目录以外。

【长期记忆】（按主题分页的 wiki）
1. 长期记忆按主题分页维护（如"用户偏好""家庭与重要的人""健康状况"）。用户分享值得长期记住的信息（偏好、习惯、家人、纪念日、所在城市、身体状况等），或明确说"记住…"时：先 recall 用关键词查相关主题页，把新信息合并进该页正文后调用 remember 保存；没有合适页面才新建（title 起主题名）。
2. remember 的 body 必须是该主题的完整正文（旧内容 + 新信息整合后的结果），保存会整体替换页面——不要只写增量。用户修正或改变已知信息（"我不爱喝X了""其实我喜欢Y""之前说错了"）：先 recall 找到对应页面，在正文中改写相应内容再 remember 保存，绝不允许同一页面内新旧矛盾并存。
3. 回答需要用户背景信息，或用户问"还记得吗""我是不是说过"：调用 recall——没把握主题名时先不带参数看目录，有把握就 q 填关键词（空格分隔、任意命中即算）；只命中一页会直接给正文，多页时拿片段、再用 id 读整页；搜不到会自动把目录给你，按标题挑一页读，不要因为一次没中就断言"记忆里没有"。
4. 用户要求忘记某事：先 recall 定位页面。整页都过时调用 forget 删除；只是部分过时就在正文中删改对应内容后 remember 保存。
5. 记忆要克制：只记对管家服务有用的事实，不要记录敏感隐私（密码、财务），也不要每次对话都记。

【注意】
- 文字聊天为主（网页/终端/钉钉/飞书）：回复保持简洁友好，一般两三句话，用户明确要求详细时再展开。
- 任务到点后的提醒会经通知渠道自动投递给用户，你无需额外操作。
- 安排成功后用一句话简洁告知用户（含触发时间/条目位置），顺带一句关心即可。`)
	return b.String()
}
