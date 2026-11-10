// aggregate.go 是一轮 turn 的事件聚合器：把 bridge 入队的 raw SessionUpdate
// 收敛成时间线可承载的少量事件。不聚合的代价是实测数字——一轮 54.6s 的作业 =
// 153 条 update（80 条 thought + 52 条 message 碎片 + 单条 25003 B 的
// available_commands_update），直接透传会把网页时间线刷爆。
//
// flush 规则：文本/思考按「≥1200 字 或 距上次 flush ≥3s 或 turn 结束」成段投出；
// tool_call 只取 kind + title（不取 locations——C11 实测上报假路径），状态映射到
// 主聊天时间线的 run/ok/fail；usage_update 不投时间线但记录水位（M-5，随投影
// 落盘），单轮 in/out 记账以 PromptResponse.Usage 为准。
//
// 一次工具调用会被 agent 拆成多条 update：首条 tool_call 带 title/status，
// 之后大量 tool_call_update 只带 toolCallId + 进度内容（codebuddy 实测不带
// status）。时间线按 toolCallId 归并、同状态只投一次——否则每次进度更新都会
// 新起一个转圈的"工具调用"，而最终 completed 只能回填其中一行。
package acp

import (
	"encoding/json"
	"strings"
	"sync/atomic"
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
	toolStatus map[string]string // toolCallId → 已投出的状态（同状态不重复投）
	toolDetail map[string]string // toolCallId → 已投出的详情（参数/结果）
	toolInput  map[string]string // toolCallId → 最近参数（args；codebuddy 在进度 update 里才给）
	toolOutput map[string]string // toolCallId → 最近输出（content/rawOutput，可能只在进度里给）

	used         int     // 最近一次 usage_update：当前上下文占用（M-5）
	size         int     // 窗口总量
	costAmount   float64 // 会话累计花费（agent 自报，仅展示——C10 纪律：不进预算）
	costCurrency string

	// 非标准扩展：部分 agent 把真实用量塞在 usage_update._meta.usage 里（见
	// usage_caps.go）。一轮内每次模型调用各一条，按 messageId/requestId 去重后
	// 在轮末求和——只在这类 agent 上启用。
	caps       usageCaps
	metaUsages map[string]metaUsage

	In, Out, Cached int64
	rawUpdates      int
	dropped         int64

	// quiet 表示"这一轮还没收到任何 update"。看门狗在另一条 goroutine 上读它，
	// 所以必须是 atomic（handle() 只在 pump goroutine 上写）。
	quiet atomic.Bool
}

// sawNothing 报告到目前为止这一轮有没有收到过任何动静（文本/工具/用量都算）。
func (s *turnSink) sawNothing() bool { return s.quiet.Load() }

// allText 返回本轮全部回答文本。
func (s *turnSink) allText() string { return s.all.String() }

// context 报告 agent 最近一次 usage_update 的上下文水位（未上报时 size 为 0）。
func (s *turnSink) context() (used, size int, costAmount float64, costCurrency string) {
	return s.used, s.size, s.costAmount, s.costCurrency
}

func newTurnSink(hooks TurnHooks, agent string) *turnSink {
	s := &turnSink{hooks: hooks, startedAt: time.Now(), caps: usageCapsFor(agent),
		toolTitles: map[string]string{}, toolStatus: map[string]string{}, toolDetail: map[string]string{},
		toolInput: map[string]string{}, toolOutput: map[string]string{}}
	s.quiet.Store(true)
	return s
}

// handle 消费一条 update（在 turn 的 pump goroutine 上串行调用）。
func (s *turnSink) handle(n acp.SessionNotification) {
	s.rawUpdates++
	s.quiet.Store(false) // 收到动静，看门狗不再报"没回音"
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
		id := string(u.ToolCall.ToolCallId)
		if u.ToolCall.Title != "" {
			s.toolTitles[id] = u.ToolCall.Title
		}
		// 展开区：参数优先，其次工具自带的 content（部分 agent 把摘要放这里）。
		input := summarizeJSON(u.ToolCall.RawInput, toolDetailLimit)
		if input == "" {
			input = summarizeToolContent(u.ToolCall.Content, toolDetailLimit)
		}
		if input != "" {
			s.toolInput[id] = input
		}
		s.emitTool(id, u.ToolCall.Title, string(u.ToolCall.Kind), string(u.ToolCall.Status), input)
	case u.ToolCallUpdate != nil:
		id := string(u.ToolCallUpdate.ToolCallId)
		title := ""
		if u.ToolCallUpdate.Title != nil {
			title = *u.ToolCallUpdate.Title
			s.toolTitles[id] = title
		} else {
			title = s.toolTitles[id] // 沿用该工具调用首次上报的标题
		}
		kind := ""
		if u.ToolCallUpdate.Kind != nil {
			kind = string(*u.ToolCallUpdate.Kind)
		}
		// 参数/输出在 codebuddy 的进度 update 里才出现（且不带 status）。
		input := summarizeJSON(u.ToolCallUpdate.RawInput, toolDetailLimit)
		prevInput := s.toolInput[id]
		if input != "" {
			s.toolInput[id] = input
		}
		output := summarizeJSON(u.ToolCallUpdate.RawOutput, toolDetailLimit)
		if output == "" {
			output = summarizeToolContent(u.ToolCallUpdate.Content, toolDetailLimit)
		}
		if output != "" {
			s.toolOutput[id] = output
		}
		if u.ToolCallUpdate.Status != nil {
			detail := output
			if detail == "" {
				detail = s.toolOutput[id] // 完成事件常不带内容，沿用进度里见过的输出
			}
			s.emitTool(id, title, kind, string(*u.ToolCallUpdate.Status), detail)
			break
		}
		// 不带 status 的进度更新不新起行；参数首次到位时原地刷新展开区。
		// 已收尾的调用不再因迟到的进度改状态或内容。
		if input != "" && input != prevInput && s.toolStatus[id] == "run" {
			s.emitTool(id, title, kind, "in_progress", input)
		}
	case u.UsageUpdate != nil:
		// M-5：usage_update 是上下文水位（size=窗口总量、used=当前占用）加可选
		// 的会话累计 cost——不是单轮 in/out。记到 sink，turn 结束随投影落盘；
		// 单轮记账仍以 PromptResponse.Usage 为准（usage.Record 维度不变）。
		// 唯一的例外是白名单里的 agent（usage_caps.go）：它们不给响应级 usage，
		// 真实用量只在这个通知的 _meta 里。
		s.used = u.UsageUpdate.Used
		s.size = u.UsageUpdate.Size
		if u.UsageUpdate.Cost != nil {
			s.costAmount = u.UsageUpdate.Cost.Amount
			s.costCurrency = u.UsageUpdate.Cost.Currency
		}
		if s.caps.meta {
			s.absorbMetaUsage(u.UsageUpdate.Meta)
		}
	default:
		// available_commands_update（单条实测 25KB，含用户 skill 全表）、
		// config_option_update、session_info_update、plan_* 等一律丢弃。
	}
}

// finish 在 turn 结束（Prompt 返回）后调用：清空残留缓冲并归集结果。
func (s *turnSink) finish() {
	s.flushNow()
}

// metaUsage 是一次模型调用的真实用量（来自 usage_update._meta.usage）。
type metaUsage struct {
	In, Out, CacheRead, CacheWrite int64
}

// absorbMetaUsage 从 usage_update._meta 里取出 usage 并记录。
// 字段名各家不同（OpenAI 风格 prompt_tokens/completion_tokens，也有 camelCase），
// 这里按常见别名取值；取不到就跳过，不猜。
// 同一轮里同一 messageId 会重复上报（流式中途一次、收尾一次，数字递增），
// 以最后一次为准；不同 messageId 是不同模型调用，轮末求和。
func (s *turnSink) absorbMetaUsage(meta map[string]any) {
	if meta == nil {
		return
	}
	raw, ok := meta["usage"].(map[string]any)
	if !ok {
		return
	}
	in := metaInt(raw, "prompt_tokens", "inputTokens", "input_tokens", "promptTokens")
	out := metaInt(raw, "completion_tokens", "outputTokens", "output_tokens", "completionTokens")
	read := metaInt(raw, "prompt_cache_hit_tokens", "cache_read_input_tokens", "cachedReadTokens", "cached_tokens")
	write := metaInt(raw, "cache_creation_input_tokens", "cacheWriteInputTokens", "cachedWriteTokens")
	// prompt_tokens 含缓存命中部分；ACP 的 In 语义是「未命中缓存的输入」，
	// 否则缓存命中率的分母会把命中数算两遍。
	if miss := metaInt(raw, "prompt_cache_miss_tokens"); miss > 0 {
		in = miss
	} else if in > 0 && read > 0 {
		in -= read
	}
	if in <= 0 && out <= 0 {
		return
	}
	key, _ := meta["codebuddy.ai/messageId"].(string)
	if key == "" {
		key, _ = meta["codebuddy.ai/requestId"].(string)
	}
	if key == "" {
		key = "default"
	}
	if s.metaUsages == nil {
		s.metaUsages = map[string]metaUsage{}
	}
	s.metaUsages[key] = metaUsage{In: in, Out: out, CacheRead: read, CacheWrite: write}
}

// metaTotals 汇总本轮所有模型调用的用量（没有就返回零值）。
func (s *turnSink) metaTotals() metaUsage {
	var t metaUsage
	for _, u := range s.metaUsages {
		t.In += u.In
		t.Out += u.Out
		t.CacheRead += u.CacheRead
		t.CacheWrite += u.CacheWrite
	}
	return t
}

// metaInt 按别名顺序取第一个正数（JSON 解码后都是 float64）。
func metaInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if v > 0 {
				return int64(v)
			}
		case int64:
			if v > 0 {
				return v
			}
		case int:
			if v > 0 {
				return int64(v)
			}
		}
	}
	return 0
}

// flushNow 立即投出文本/思考缓冲。工具步骤前必须先调：文本是按 3s/1200 字
// 成段投的，缓冲里可能已有"工具之前"写下的半句话；若先投步骤，这段话会在
// 工具行之后才到，时间线上就成了被工具调用从中间切开的残句。
func (s *turnSink) flushNow() {
	now := time.Now()
	if s.text.Len() > 0 {
		s.emit(s.text.String(), s.hooks.OnText)
		s.text.Reset()
		s.textAt = now
	}
	if s.thought.Len() > 0 {
		s.emit(s.thought.String(), s.hooks.OnThought)
		s.thought.Reset()
		s.thoughtAt = now
	}
}

// emitTool 按 toolCallId 归并一次工具调用：同 id 同状态同详情只投一次，
// 让前端始终「一行 = 一次调用」，run → ok/fail 在原行上收敛。
// 完成事件若带来结果详情（同状态但新内容），允许再投一次让展开区更新。
func (s *turnSink) emitTool(id, title, kind, rawStatus, detail string) {
	status := mapStatus(rawStatus)
	if id != "" {
		if s.toolStatus[id] == status && s.toolDetail[id] == detail {
			return
		}
		s.toolStatus[id] = status
		s.toolDetail[id] = detail
	}
	s.flushNow() // 工具调用是时间线边界：先把之前的文本落到它前面
	s.emitStep(id, title, kind, status, detail)
}

func (s *turnSink) emitStep(id, title, kind, status, detail string) {
	if s.hooks.OnStep == nil {
		return
	}
	// 详情优先用参数/结果；没有时退回工具类别（"other" 是 codebuddy 的占位，不展示）。
	if detail == "" && kind != "" && kind != "other" {
		detail = kind
	}
	if title == "" {
		title = kind
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80]) + "…"
	}
	// C11：kind/title 来自子 agent 自述，前端展示时带「文案来自子 agent」标注。
	s.hooks.OnStep(agent.Step{Kind: "acp", ID: id, Name: title, Detail: detail, Status: status})
}

// toolDetailLimit 是展开区参数/结果的字符上限（时间线只做线索，不做全量归档）。
const toolDetailLimit = 600

// summarizeJSON 把工具参数/输出压成一行可读文本（对象转 JSON，超长截断）。
func summarizeJSON(v any, limit int) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return truncateRunes(s, limit)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	switch s {
	case "", "{}", "[]", "null", `""`:
		return ""
	}
	return truncateRunes(s, limit)
}

// summarizeToolContent 汇总标准 ToolCallContent（文本 / 改文件 / 终端引用）。
func summarizeToolContent(items []acp.ToolCallContent, limit int) string {
	var parts []string
	for _, it := range items {
		switch {
		case it.Content != nil:
			if t := strings.TrimSpace(blockText(it.Content.Content)); t != "" {
				parts = append(parts, t)
			}
		case it.Diff != nil:
			parts = append(parts, "修改 "+it.Diff.Path)
		case it.Terminal != nil:
			parts = append(parts, "终端 "+it.Terminal.TerminalId)
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), limit)
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
// cancelled 不在 SDK 枚举里，但 codebuddy 拒权时会发，按失败收尾。
func mapStatus(s string) string {
	switch s {
	case "completed":
		return "ok"
	case "failed", "cancelled":
		return "fail"
	default: // pending / in_progress / 空
		return "run"
	}
}
