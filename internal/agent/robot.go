// Package agent 把 vortexagent 框架组装成机器人：创建 LLM Provider、注册工具、
// 配置生命周期钩子（日志/拦截）、注入面向定时提醒场景的中文系统提示词。
package agent

import (
	"context"
	"fmt"
	"log"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/reminder"
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

// buildHooks 配置生命周期钩子：记录 LLM 调用与工具调用，拦截失败。
func buildHooks() *vxagent.Hooks {
	return &vxagent.Hooks{
		OnLLMCall: func(_ context.Context, info vxagent.LLMCallInfo) {
			log.Printf("[llm] round=%d model=%s tokens=%d+%d err=%v",
				info.Round, info.Model, info.Usage.InputTokens, info.Usage.OutputTokens, info.Err)
		},
		OnBeforeToolCall: func(_ context.Context, name string, args map[string]any) error {
			log.Printf("[tool] -> %s %v", name, args)
			return nil
		},
		OnAfterToolCall: func(_ context.Context, name string, _ map[string]any, result string, err error) {
			if err != nil {
				log.Printf("[tool] <- %s 失败: %v", name, err)
				return
			}
			log.Printf("[tool] <- %s: %s", name, truncate(result, 120))
		},
		OnError: func(_ context.Context, err error) {
			log.Printf("[agent] 错误: %v", err)
		},
	}
}

// RegisterTools 把机器人全部业务工具注册进 Registry。
// engine 为提醒排程器；speaker/displayer 为输出实现（开发用 console，后续换 TTS/OLED）；
// player 为音频播放器（语音模式用 portaudio，文本模式用 console 兜底）。
func RegisterTools(r *vxagent.Registry, engine *reminder.Engine, speaker output.Speaker, displayer output.Displayer, player audio.Player) error {
	tools := []vxagent.Tool{
		&tools_pkg.ScheduleReminderTool{Engine: engine},
		&tools_pkg.ListRemindersTool{Engine: engine},
		&tools_pkg.CancelReminderTool{Engine: engine},
		&tools_pkg.ScheduleRecurringTool{Engine: engine},
		&tools_pkg.ListRecurringTool{Engine: engine},
		&tools_pkg.CancelRecurringTool{Engine: engine},
		&tools_pkg.GetTimeTool{},
		&tools_pkg.SpeakNowTool{Speaker: speaker},
		&tools_pkg.DisplayNowTool{Displayer: displayer},
		&tools_pkg.PlayAudioTool{Player: player},
		&tools_pkg.PlayEffectTool{Player: player},
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
1. 用户要求"X 分钟后提醒我做 Y"：调用 schedule_reminder，相对时长填 in（如 10分钟/2小时/1天/90s），内容填 text。
2. 用户要求在具体时刻提醒（如"4点11分提醒我""明天早上8点叫我起床"）：必须先调用 get_time 获取当前时间，然后调用 schedule_reminder 时必须用 when 填 RFC3339 绝对时间（含时区，如 2026-09-05T16:11:00+08:00），禁止用 in 表达具体时刻。
3. 用户要求每天/每周固定时间做事（如"每天早上8点提醒我喝水"）：调用 schedule_recurring，自然语言时间填 when（如"每天早上8点""每周一三五9:30"）。
4. 查看/取消提醒或循环任务：调用 list_reminders / cancel_reminder / list_recurring / cancel_recurring（先列出拿 ID 再取消）。
5. 用户要求立即播报/显示：调用 speak_now（语音）和 display_now（屏幕）。
6. 用户想听声音：播放本地 wav 文件调用 play_audio（path 填文件路径）；要提示音调用 play_effect（ding/dingdong/alert）。
7. 查询当前时间调用 get_time。

【注意】
- 提醒到点后的播报由系统自动完成，你无需额外操作。
- 安排成功后用一句话简洁告知用户，并顺带一句关心。`
}

// NewSession 创建一个新的对话会话。
func NewSession() *sessionstore.Session {
	return sessionstore.NewSession("robot")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
