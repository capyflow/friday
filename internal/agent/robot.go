// Package agent 把 vortexagent 框架组装成机器人：创建 LLM Provider、注册工具、
// 配置生命周期钩子（日志/拦截）、注入面向陪伴与定时任务场景的中文系统提示词。
package agent

import (
	"context"
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	tools_pkg "github.com/capyflow/friday/internal/tools"
)

// Build 创建并返回配置好的 Agent，同时返回一个新建的会话。
// onDelta 为流式输出回调（可为 nil）。
func Build(cfg *config.Config, registry *vxagent.Registry, onDelta func(llm.Delta)) (*vxagent.Agent, error) {
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

	ag := vxagent.New(vxagent.Options{
		Provider:     provider,
		Registry:     registry,
		SystemPrompt: systemPrompt(cfg.SystemPrompt),
		OnDelta:      onDelta,
		Hooks:        buildHooks(),
	})
	return ag, nil
}

// buildHooks 配置生命周期钩子：按请求 ID 记录完整调用链——LLM 每轮调用（轮次/
// token/耗时）、模型决定调用的工具、工具执行结果、最终回答与错误。
// 请求 ID 从 ctx 读取（入口处由 logx.WithID 注入，见 logx 包）。
func buildHooks() *vxagent.Hooks {
	return &vxagent.Hooks{
		OnLLMCall: func(ctx context.Context, info vxagent.LLMCallInfo) {
			logx.Printf(ctx, "[llm] round=%d model=%s tokens=%d+%d 耗时=%s err=%v",
				info.Round, info.Model, info.Usage.InputTokens, info.Usage.OutputTokens,
				info.Duration, info.Err)
		},
		OnMessage: func(ctx context.Context, msg llm.Message) {
			// 工具调用轮次：记录模型决定调用哪些工具及参数；
			// 纯文本的最终回答由 OnFinish 记录，这里不重复。
			for _, tc := range msg.ToolCalls {
				logx.Printf(ctx, "[llm] 请求工具: %s %v", tc.Name, tc.Arguments)
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
				return
			}
			logx.Printf(ctx, "[tool] <- %s: %s", name, logx.Truncate(result, 120))
		},
		OnFinish: func(ctx context.Context, answer string) {
			logx.Printf(ctx, "[agent] 回答: %s", logx.Truncate(answer, 200))
		},
		OnError: func(ctx context.Context, err error) {
			logx.Printf(ctx, "[agent] 错误: %v", err)
		},
	}
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

// RegisterTools 把机器人全部业务工具注册进 Registry。
// engine 为任务排程器（单次/多次/循环）；mem 为长期记忆仓库；speaker/displayer
// 为输出实现（开发用 console，后续换 TTS/OLED）；player 为音频播放器
// （语音模式用 portaudio，文本模式用 console 兜底）；
// chatCtrl 承载 stop_listening 工具的结束信号（语音连续对话用，文本模式无人读取）。
func RegisterTools(r *vxagent.Registry, engine *task.Engine, mem *memory.Store, speaker output.Speaker, displayer output.Displayer, player audio.Player, chatCtrl *tools_pkg.ChatController) error {
	tools := []vxagent.Tool{
		&tools_pkg.ScheduleTaskTool{Engine: engine},
		&tools_pkg.ListTasksTool{Engine: engine},
		&tools_pkg.CancelTaskTool{Engine: engine},
		&tools_pkg.RememberTool{Memory: mem},
		&tools_pkg.RecallTool{Memory: mem},
		&tools_pkg.ForgetTool{Memory: mem},
		&tools_pkg.GetTimeTool{},
		&tools_pkg.SpeakNowTool{Speaker: speaker},
		&tools_pkg.DisplayNowTool{Displayer: displayer},
		&tools_pkg.PlayAudioTool{Player: player},
		&tools_pkg.PlayEffectTool{Player: player},
		&tools_pkg.StopChatTool{Ctrl: chatCtrl},
	}
	for _, t := range tools {
		if err := r.Add(t); err != nil {
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
5. 查看/取消任务：调用 list_tasks / cancel_task（先列出拿 ID 再取消，单次/多次/循环通用）。
6. 用户要求立即播报/显示：调用 speak_now（语音）和 display_now（屏幕）。
7. 用户想听声音：播放本地 wav 文件调用 play_audio（path 填文件路径）；要提示音调用 play_effect（ding/dingdong/alert）。
8. 查询当前时间调用 get_time。
9. 连续对话：你回答后会继续听用户说下一句，用户无需重复唤醒词；当用户明确表示结束（如"不用了""别说了""就这样""再见"）时，调用 stop_listening 工具结束本轮对话，其他情况不要调用。

【长期记忆】
1. 用户分享值得长期记住的信息（偏好、习惯、家人、纪念日、身体状况等），或明确说"记住…"：调用 remember，content 用一句话概括（如"用户对花粉过敏"）。
2. 回答需要用户背景信息，或用户问"还记得吗""我是不是说过"：调用 recall，q 填关键词；留空返回最近的记忆。
3. 用户要求忘记某事：先 recall 拿到记忆 ID，再调用 forget。
4. 记忆要克制：只记对陪伴有用的事实，不要记录敏感隐私（密码、财务），也不要每次对话都记。

【注意】
- 语音交互为主：回复保持简短口语化，一般一两句话（50 字以内），方便语音播报，用户明确要求详细时再展开。
- 任务到点后的播报由系统自动完成，你无需额外操作。
- 安排成功后用一句话简洁告知用户，并顺带一句关心。`
}

// NewSession 创建一个新的对话会话。
func NewSession() *sessionstore.Session {
	return sessionstore.NewSession("robot")
}
