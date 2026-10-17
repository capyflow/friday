// aggregate.go 是一轮 turn 的事件聚合器：把 bridge 入队的 raw SessionUpdate
// 收敛成时间线可承载的少量事件。不聚合的代价是实测数字——一轮 54.6s 的作业 =
// 153 条 update（80 条 thought + 52 条 message 碎片 + 单条 25003 B 的
// available_commands_update），直接透传会把网页时间线刷爆。
//
// flush 规则：文本/思考按「≥1200 字 或 距上次 flush ≥3s 或 turn 结束」成段投出；
// tool_call 只取 kind + title（不取 locations——C11 实测上报假路径），状态映射到
// 主聊天时间线的 run/ok/fail；usage_update 不投时间线（记账以 PromptResponse.Usage
// 为准，避免会话级累计被重复计入单轮）。
package acp

import (
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/agent"
)

const (
	flushChars = 1200
	flushEvery = 3 * time.Second
)

// TurnHooks 是一轮对话的对外回调（SSE 写出方注入；均可为 nil）。
type TurnHooks struct {
	OnText    func(string)     // 回答文本增量（成段）
	OnThought func(string)     // 思考流增量（成段；D5 默认开，设置可关）
	OnStep    func(agent.Step) // 工具调用过程事件
	OnStatus  func(string)     // 生命周期占位（启动中/重接中/上下文已重置…）
}

// status 安全投递一条生命周期占位（OnStatus 未注入时丢弃）。
func (h TurnHooks) status(msg string) {
	if h.OnStatus != nil {
		h.OnStatus(msg)
	}
}

// turnSink 聚合一轮 update。
type turnSink struct {
	hooks TurnHooks

	text       strings.Builder // 当前未 flush 的文本缓冲
	thought    strings.Builder
	all        strings.Builder // 全部回答文本留档（投影重拼用，含已 flush 段）
	textAt     time.Time       // 上次文本 flush 时刻
	thoughtAt  time.Time
	startedAt  time.Time
	toolTitles map[string]string // toolCallId → 标题（update 常只带 id 不带标题）

	In, Out, Cached int64
	rawUpdates      int
	dropped         int64
}

// allText 返回本轮全部回答文本。
func (s *turnSink) allText() string { return s.all.String() }

func newTurnSink(hooks TurnHooks) *turnSink {
	return &turnSink{hooks: hooks, startedAt: time.Now(), toolTitles: map[string]string{}}
}

// handle 消费一条 update（在 turn 的 pump goroutine 上串行调用）。
func (s *turnSink) handle(n acp.SessionNotification) {
	s.rawUpdates++
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if txt := blockText(u.AgentMessageChunk.Content); txt != "" {
			s.all.WriteString(txt)
			s.text.WriteString(txt)
			s.maybeFlush(&s.text, &s.textAt, s.hooks.OnText)
		}
	case u.AgentThoughtChunk != nil:
		if txt := blockText(u.AgentThoughtChunk.Content); txt != "" {
			s.thought.WriteString(txt)
			s.maybeFlush(&s.thought, &s.thoughtAt, s.hooks.OnThought)
		}
	case u.ToolCall != nil:
		if u.ToolCall.Title != "" {
			s.toolTitles[string(u.ToolCall.ToolCallId)] = u.ToolCall.Title
		}
		s.emitStep(u.ToolCall.Title, string(u.ToolCall.Kind), mapStatus(string(u.ToolCall.Status)))
	case u.ToolCallUpdate != nil:
		id := string(u.ToolCallUpdate.ToolCallId)
		title := ""
		if u.ToolCallUpdate.Title != nil {
			title = *u.ToolCallUpdate.Title
			s.toolTitles[id] = title
		} else {
			title = s.toolTitles[id] // 沿用该工具调用首次上报的标题
		}
		status := ""
		if u.ToolCallUpdate.Status != nil {
			status = string(*u.ToolCallUpdate.Status)
		}
		kind := ""
		if u.ToolCallUpdate.Kind != nil {
			kind = string(*u.ToolCallUpdate.Kind)
		}
		s.emitStep(title, kind, mapStatus(status))
	default:
		// available_commands_update（单条实测 25KB，含用户 skill 全表）、
		// config_option_update、session_info_update、plan_* 等一律丢弃。
	}
}

// finish 在 turn 结束（Prompt 返回）后调用：清空残留缓冲并归集结果。
func (s *turnSink) finish() {
	if s.text.Len() > 0 {
		s.emit(s.text.String(), s.hooks.OnText)
		s.text.Reset()
	}
	if s.thought.Len() > 0 {
		s.emit(s.thought.String(), s.hooks.OnThought)
		s.thought.Reset()
	}
}

func (s *turnSink) emitStep(title, kind, status string) {
	if s.hooks.OnStep == nil {
		return
	}
	if title == "" {
		title = kind
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80]) + "…"
	}
	// C11：kind/title 来自子 agent 自述，前端展示时带「文案来自子 agent」标注。
	s.hooks.OnStep(agent.Step{Kind: "acp", Name: title, Detail: kind, Status: status})
}

func (s *turnSink) maybeFlush(buf *strings.Builder, last *time.Time, emit func(string)) {
	now := time.Now()
	if buf.Len() >= flushChars || now.Sub(*last) >= flushEvery {
		s.emit(buf.String(), emit)
		buf.Reset()
		*last = now
	}
}

func (s *turnSink) emit(text string, emit func(string)) {
	if emit == nil || text == "" {
		return
	}
	emit(text)
}

// blockText 提取内容块中的文本（图片/资源链接等非文本块返回空）。
func blockText(c acp.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}

// mapStatus 把 ACP 的 ToolCallStatus 映射到主聊天时间线的三态。
func mapStatus(s string) string {
	switch s {
	case "completed":
		return "ok"
	case "failed":
		return "fail"
	default: // pending / in_progress / 空
		return "run"
	}
}
