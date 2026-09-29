// Package agent 把 vortexagent 框架组装成 Friday 个人管家：创建 LLM Provider、
// 注册工具、配置生命周期钩子（日志/拦截）、注入面向生活管家场景的中文系统提示词。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/ctxengine"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	tools_pkg "github.com/capyflow/friday/internal/tools"
	"github.com/capyflow/friday/internal/todo"
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
	})
	return ag, nil
}

// newProvider 按配置构建 LLM provider（Build 与上下文引擎摘要器共用）。
// 密钥允许为空（未配置不影响启动，管理后台/待办/任务照常可用）：
// LLM 请求在配置密钥前会失败并提示去「设置」页配置。
func newProvider(cfg *config.Config) (llm.Provider, error) {
	apiKey := cfg.ProviderAPIKey()
	temp := cfg.Provider.Temperature
	return llm.NewProvider(llm.ProviderConfig{
		Name:        "openai",
		APIKey:      apiKey,
		BaseURL:     cfg.Provider.BaseURL,
		Model:       cfg.Provider.Model,
		MaxTokens:   cfg.Provider.MaxTokens,
		Temperature: &temp,
	})
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
			usage.Add(int64(info.Usage.InputTokens), int64(info.Usage.OutputTokens))
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
			return nil
		},
		OnAfterToolCall: func(ctx context.Context, name string, _ map[string]any, result string, err error) {
			if err != nil {
				logx.Printf(ctx, "[tool] <- %s 失败: %v", name, err)
				trace.Record(ctx, "tool", "工具 %s 执行失败：%v", name, err)
				return
			}
			logx.Printf(ctx, "[tool] <- %s: %s", name, logx.Truncate(result, 120))
			trace.Record(ctx, "tool", "工具 %s 返回：%s", name, logx.Truncate(result, 500))
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
	Engine    *task.Engine     // 任务排程器（单次/多次/循环/智能任务）
	Todos     *todo.Store      // 待办清单仓库
	Memory    *memory.WikiStore // 长期记忆 wiki 仓库（主题页面）
	Ctx       *ContextManager  // 上下文引擎管理器（注册检索工具用，可为 nil）
	Speaker   output.Speaker   // 播报输出（硬件输出开启时使用）
	Displayer output.Displayer // 屏幕输出（硬件输出开启时使用）
	Player    audio.Player     // 音频播放器（硬件输出开启时使用）
	Config    *config.Config   // 运行配置（文件白名单、硬件开关、天气默认城市等）
	Approver  *approval.Queue  // 危险操作审批队列（权限层 ApprovalUI 桥接与 write/edit 用，可为 nil）
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
		&tools_pkg.ListTasksTool{Engine: d.Engine},
		&tools_pkg.CancelTaskTool{Engine: d.Engine},
		&tools_pkg.AddTodoTool{Store: d.Todos},
		&tools_pkg.ListTodosTool{Store: d.Todos},
		&tools_pkg.CompleteTodoTool{Store: d.Todos},
		&tools_pkg.DeleteTodoTool{Store: d.Todos},
		&tools_pkg.WeatherTool{DefaultCity: d.Config.Weather.DefaultCity},
		&tools_pkg.RememberTool{Memory: d.Memory},
		&tools_pkg.RecallTool{Memory: d.Memory},
		&tools_pkg.ForgetTool{Memory: d.Memory},
		&tools_pkg.GetTimeTool{},
		&tools_pkg.ReadFileTool{FileTools: fileTools},
		&tools_pkg.ListFilesTool{FileTools: fileTools},
		&tools_pkg.WriteFileTool{FileTools: fileTools, Approver: approver},
		&tools_pkg.EditFileTool{FileTools: fileTools, Approver: approver},
	}
	if d.Config.Hardware.Enabled {
		tools = append(tools,
			&tools_pkg.SpeakNowTool{Speaker: d.Speaker},
			&tools_pkg.DisplayNowTool{Displayer: d.Displayer},
			&tools_pkg.PlayAudioTool{Player: d.Player},
			&tools_pkg.PlayEffectTool{Player: d.Player})
	}
	for _, t := range tools {
		if err := r.Add(t); err != nil {
			return err
		}
	}
	// exec_command（框架工具 + 默认超时包装）：off 模式不注册——模型不可见即整体禁用。
	// 权限规则（白名单/毁灭性黑名单）由 Build 里的权限 Checker 承担，见 permission.go。
	if d.Config.Tools.Exec.Mode != string(approval.ModeOff) {
		if err := r.Add(NewExecCommandTool(d.Config.Tools.Exec.TimeoutSeconds())); err != nil {
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
【待办清单】
1. 用户交代要做的事但没有明确时间点（"记得买牛奶""周三前交房租"）：调用 add_todo 记入待办。
2. 用户问"我有什么待办/今天要干什么"：调用 list_todos，按清单汇报。
3. 用户做完某事（"房租交了"）：调用 complete_todo 标记完成（先用 list_todos 拿 ID）；说"这条不用了"调用 delete_todo；说"其实还没做完"调用 complete_todo 并 done=false。
4. 有明确触发时间的不要只记待办：用 schedule_task 定提醒，可以两者配合——先 add_todo 记事、再定到点提醒。

【定时任务】
1. 单次提醒（"10分钟后提醒我敷面膜"）：调用 schedule_task，in 填相对时长（如 10分钟/2小时/1天/90s），text 填内容。
2. 具体时刻提醒（"4点11分提醒我""明天早上8点叫我起床"）：必须先调用 get_time 获取当前时间，然后调用 schedule_task 时必须用 when 填 RFC3339 绝对时间（含时区，如 2026-09-05T16:11:00+08:00），禁止用 in 表达具体时刻。
3. 有限多次（"每10分钟提醒我一次，共3次"）：调用 schedule_task，in 填间隔（10分钟），times 填次数（3）。
4. 无限循环（"每天早上8点提醒我喝水""每周一三五9点播报天气"）：调用 schedule_task，every 填自然语言（"每天早上8点""每周一三五9:30"）或 cron 表达式。
5. 智能任务（"每天早上8点看看我的待办和天气，给我一份晨报"这类需要自主判断的任务）：调用 schedule_task，agent=true，text 填完整的任务指令，到点后由你自主决策执行。
6. 查看/取消任务：调用 list_tasks / cancel_task（先列出拿 ID 再取消，单次/多次/循环/智能通用）。
7. 查询当前时间调用 get_time。

【天气查询】
1. 用户问天气、要不要带伞、穿什么：调用 get_weather，city 填城市名；问"未来几天"时把预报转述清楚。
2. 用户没指明城市：配置了默认城市就直接查并说明；没配置就先问用户所在城市，用户回答后可用 remember 记入"用户偏好"页（如"用户所在城市是上海"），下次直接查。
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
3. 执行 shell 命令用 exec_command（默认 30 秒超时，可通过 timeout 参数最长调到 10 分钟）：只用于轻量、安全的命令，rm -rf 等毁灭性命令会被强制拦截。

【长期记忆】（按主题分页的 wiki）
1. 长期记忆按主题分页维护（如"用户偏好""家庭与重要的人""健康状况"）。用户分享值得长期记住的信息（偏好、习惯、家人、纪念日、所在城市、身体状况等），或明确说"记住…"时：先 recall 用关键词查相关主题页，把新信息合并进该页正文后调用 remember 保存；没有合适页面才新建（title 起主题名）。
2. remember 的 body 必须是该主题的完整正文（旧内容 + 新信息整合后的结果），保存会整体替换页面——不要只写增量。用户修正或改变已知信息（"我不爱喝X了""其实我喜欢Y""之前说错了"）：先 recall 找到对应页面，在正文中改写相应内容再 remember 保存，绝不允许同一页面内新旧矛盾并存。
3. 回答需要用户背景信息，或用户问"还记得吗""我是不是说过"：调用 recall，q 填关键词拿到相关页面（含片段），需要完整内容再传 id 读整页。
4. 用户要求忘记某事：先 recall 定位页面。整页都过时调用 forget 删除；只是部分过时就在正文中删改对应内容后 remember 保存。
5. 记忆要克制：只记对管家服务有用的事实，不要记录敏感隐私（密码、财务），也不要每次对话都记。

【注意】
- 文字聊天为主（网页/终端/钉钉/飞书）：回复保持简洁友好，一般两三句话，用户明确要求详细时再展开。
- 任务到点后的提醒会经通知渠道自动投递给用户，你无需额外操作。
- 安排成功后用一句话简洁告知用户（含触发时间/条目位置），顺带一句关心即可。`)
	return b.String()
}
