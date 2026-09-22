// Package agent 把 vortexagent 框架组装成机器人：创建 LLM Provider、注册工具、
// 配置生命周期钩子（日志/拦截）、注入面向陪伴与定时任务场景的中文系统提示词。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	tools_pkg "github.com/capyflow/friday/internal/tools"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
)

// Build 创建并返回配置好的 Agent。pool 为会话池：轨迹钩子在每轮 LLM 调用时
// 对"当前请求的会话"做输入快照，从而记录下发给模型的完整上下文。
// approver 为审批队列：构建全局权限 Checker（规则翻译 + ApprovalUI 桥接），
// nil 时按 off 语义兜底（exec_command 不注册、其余工具直通）。
// 调用方在 Build 后经 Brain 初始化会话（池按 key 惰性创建）。
func Build(cfg *config.Config, registry *vxagent.Registry, pool *SessionPool, approver *approval.Queue) (*vxagent.Agent, error) {
	apiKey := cfg.ProviderAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("未找到 API 密钥（环境变量 %s）", cfg.Provider.APIKeyEnv)
	}
	temp := cfg.Provider.Temperature
	provider, err := llm.NewProvider(llm.ProviderConfig{
		Name:        "openai",
		APIKey:      apiKey,
		BaseURL:     cfg.Provider.BaseURL,
		Model:       cfg.Provider.Model,
		MaxTokens:   cfg.Provider.MaxTokens,
		Temperature: &temp,
	})
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

	prompt := systemPrompt(cfg.SystemPrompt)
	ag := vxagent.New(vxagent.Options{
		Provider:     provider,
		Registry:     registry,
		SystemPrompt: prompt,
		Hooks:        buildHooks(pool, prompt),
		Permissions:  perms,
	})
	return ag, nil
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

// Deps 汇总 RegisterTools 需要的机器人能力依赖。
type Deps struct {
	Engine    *task.Engine     // 任务排程器（单次/多次/循环/智能任务）
	Memory    *memory.Store    // 长期记忆仓库
	Speaker   output.Speaker   // 播报输出（开发用 console）
	Displayer output.Displayer // 屏幕输出（开发用 console）
	Player    audio.Player     // 音频播放器
	Config    *config.Config   // 运行配置（文件白名单等）
	Approver  *approval.Queue  // 危险操作审批队列（权限层 ApprovalUI 桥接与 write/edit 用，可为 nil）
}

// RegisterTools 把机器人全部业务工具注册进 Registry。
// speaker/displayer 为输出实现（开发用 console，后续换 TTS/OLED）；player 为
// 音频播放器（接喇叭后用 portaudio，当前用 console 兜底）；approver 支撑
// write_file/edit_file 的审批机制（confirm 模式）与 exec_command 的全局权限层
// （见 permission.go），nil 时 exec 按 off 禁用、文件工具直行。
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
		&tools_pkg.RememberTool{Memory: d.Memory},
		&tools_pkg.RecallTool{Memory: d.Memory},
		&tools_pkg.ForgetTool{Memory: d.Memory},
		&tools_pkg.GetTimeTool{},
		&tools_pkg.SpeakNowTool{Speaker: d.Speaker},
		&tools_pkg.DisplayNowTool{Displayer: d.Displayer},
		&tools_pkg.PlayAudioTool{Player: d.Player},
		&tools_pkg.PlayEffectTool{Player: d.Player},
		&tools_pkg.ReadFileTool{FileTools: fileTools},
		&tools_pkg.ListFilesTool{FileTools: fileTools},
		&tools_pkg.WriteFileTool{FileTools: fileTools, Approver: approver},
		&tools_pkg.EditFileTool{FileTools: fileTools, Approver: approver},
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
	return nil
}

// systemPrompt 生成机器人系统提示词：陪伴型智能助手的角色定位与工具使用规则。
func systemPrompt(override string) string {
	if override != "" {
		return override
	}
	return `你是一个部署在智能硬件上的陪伴型中文智能助手，名字叫"小五"。你的职责是陪伴与辅助用户：像朋友一样聊天、关心用户的日常，同时通过工具帮用户完成具体事情。

【角色与陪伴】
1. 与用户自然对话：倾听、回应情绪、给出温暖鼓励，语气亲切自然，像熟悉的朋友。
2. 主动关心：结合时间（get_time）与用户习惯给出贴心建议（如按时吃饭、休息）。
3. 用户情绪低落时，先共情再给建议，不敷衍不说教。

【任务执行】
1. 单次提醒（"10分钟后提醒我敷面膜"）：调用 schedule_task，in 填相对时长（如 10分钟/2小时/1天/90s），text 填内容。
2. 具体时刻提醒（"4点11分提醒我""明天早上8点叫我起床"）：必须先调用 get_time 获取当前时间，然后调用 schedule_task 时必须用 when 填 RFC3339 绝对时间（含时区，如 2026-09-05T16:11:00+08:00），禁止用 in 表达具体时刻。
3. 有限多次（"每10分钟提醒我一次，共3次"）：调用 schedule_task，in 填间隔（10分钟），times 填次数（3）。
4. 无限循环（"每天早上8点提醒我喝水""每周一三五9点播报天气"）：调用 schedule_task，every 填自然语言（"每天早上8点""每周一三五9:30"）或 cron 表达式。
5. 智能任务（"每天早上8点看看我的备忘录，把今天要做的事告诉我"这类需要自主判断的任务）：调用 schedule_task，agent=true，text 填完整的任务指令，到点后由你自主决策执行。
6. 查看/取消任务：调用 list_tasks / cancel_task（先列出拿 ID 再取消，单次/多次/循环/智能通用）。
7. 用户要求立即播报/显示：调用 speak_now（播报）和 display_now（屏幕显示）。
8. 用户想听声音：播放本地 wav 文件调用 play_audio（path 填文件路径）；要提示音调用 play_effect（ding/dingdong/alert）。
9. 查询当前时间调用 get_time。

【文件与命令】
1. 读文件用 read_file（可分段续读），列目录用 list_files；写文件用 write_file（原文件自动备份为 .bak）；精确修改用 edit_file（old 必须唯一匹配，防止误改）。
2. write_file/edit_file/exec_command 属于受控操作：confirm 审批模式下需用户在管理后台批准后才会执行，请把要做什么说清楚；收到"用户未批准"或"被权限系统拒绝"时不要反复重试同一操作。
3. 执行 shell 命令用 exec_command（默认 30 秒超时，可通过 timeout 参数最长调到 10 分钟）：只用于轻量、安全的命令，rm -rf 等毁灭性命令会被强制拦截。

【长期记忆】
1. 用户分享值得长期记住的信息（偏好、习惯、家人、纪念日、身体状况等），或明确说"记住…"：调用 remember，content 用一句话概括（如"用户对花粉过敏"）。
2. 用户修正或改变已知信息（如"我不爱喝X了""其实我喜欢Y""之前说错了"）：必须先 recall 相关关键词，找到与之矛盾的旧记忆，再用 remember 传 replaceId=旧记忆ID 覆盖（或先 forget 旧条目），绝不允许新旧矛盾的记忆并存。
3. 回答需要用户背景信息，或用户问"还记得吗""我是不是说过"：调用 recall，q 填关键词；留空返回最近的记忆。
4. 用户要求忘记某事：先 recall 拿到记忆 ID，再调用 forget。
5. 记忆要克制：只记对陪伴有用的事实，不要记录敏感隐私（密码、财务），也不要每次对话都记。

【注意】
- 文字聊天为主（网页/终端）：回复保持简洁友好，一般两三句话，用户明确要求详细时再展开。
- 任务到点后的播报由系统自动完成，你无需额外操作。
- 安排成功后用一句话简洁告知用户，并顺带一句关心。`
}
