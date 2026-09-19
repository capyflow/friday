// Package trace 保存 agent 的调用轨迹：每次用户输入或任务触发分配一个请求 ID，
// 沿途的 LLM 调用（轮次/token/耗时）、工具调用与结果、最终回答、错误按请求归档。
// 数据存内存环形缓冲（最近 maxRequests 条），重启即清空，仅供管理后台查看。
package trace

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/logx"
)

const (
	// maxRequests 环形缓冲保留的请求数上限，超出淘汰最旧的。
	maxRequests = 200
	// maxEntries 单个请求的轨迹条目上限（防御工具循环等异常场景）。
	maxEntries = 500
	// maxText 单条轨迹文本的最大长度（rune）。
	maxText = 2000
)

// Entry 是轨迹中的一条事件。In/Out/Ms 仅 LLM 相关事件使用：
// 本轮发给模型的输入 token、模型输出的 token、调用耗时（毫秒）。
type Entry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // llm_in / llm_out / tool_req / tool / answer / error / task
	Text string    `json:"text"`
	In   int64     `json:"in,omitempty"`
	Out  int64     `json:"out,omitempty"`
	Ms   int64     `json:"ms,omitempty"`
}

// Request 是一次完整调用的轨迹。In/Out 为该次请求所有 LLM 轮次的 token 累计。
type Request struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`            // web / repl / task / dingtalk / feishu ...
	Session string    `json:"session,omitempty"` // 会话 key（多会话区分归属）
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
	Input   string    `json:"input"`
	Answer  string    `json:"answer,omitempty"`
	Error   string    `json:"error,omitempty"`
	In      int64     `json:"in,omitempty"`
	Out     int64     `json:"out,omitempty"`
	Entries []Entry   `json:"entries,omitempty"`

	// pend* 是 llm_in 记录后暂存的当轮用量，由紧随其后的 llm_out 带走。
	pendIn, pendOut, pendMs int64
}

var (
	mu    sync.Mutex
	reqs  = map[string]*Request{}
	order []string // 插入顺序（旧→新），用于淘汰与倒序列出
	// llmInCap：llm_in / llm_out 事件文本（完整上下文快照）的截断上限，
	// 比普通事件宽松——这是排查 agent 决策链的核心数据。
	llmTextCap = 6000
)

// recordWithCap 按 cap 截断并追加事件（调用方须持有 mu）。
func recordWithCap(r *Request, kind, text string, in, out, ms int64) {
	if len(r.Entries) >= maxEntries {
		return
	}
	r.Entries = append(r.Entries, Entry{Time: time.Now(), Kind: kind, Text: text, In: in, Out: out, Ms: ms})
}

// Begin 登记一次调用的开始（来源 + 会话 key + 原始输入）。
func Begin(id, source, session, input string) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := reqs[id]; ok {
		return
	}
	reqs[id] = &Request{
		ID:      id,
		Source:  source,
		Session: session,
		Started: time.Now(),
		Input:   logx.Truncate(input, maxText),
	}
	order = append(order, id)
	for len(order) > maxRequests {
		delete(reqs, order[0])
		order = order[1:]
	}
}

// Record 追加一条轨迹事件；ctx 携带请求 ID（logx.WithID）时生效。
// kind 为 answer / error 时同步填充 Request 的对应字段（留空不覆盖）。
func Record(ctx context.Context, kind, format string, args ...any) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	text := logx.Truncate(fmt.Sprintf(format, args...), maxText)
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	if len(r.Entries) >= maxEntries {
		return
	}
	r.Entries = append(r.Entries, Entry{Time: time.Now(), Kind: kind, Text: text})
	switch kind {
	case "answer":
		if r.Answer == "" {
			r.Answer = text
		}
	case "error":
		if r.Error == "" {
			r.Error = text
		}
	}
}

// RecordLLMCall 记录一轮 LLM 调用的输入快照（发给模型的完整 messages JSON）
// 与 token 用量。快照在 OnLLMCall 时机截取：此刻会话内容恰好等于本轮输入。
func RecordLLMCall(ctx context.Context, round int, inputJSON string, in, out, ms int64) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	r.In += in
	r.Out += out
	r.pendIn, r.pendOut, r.pendMs = in, out, ms
	if len(r.Entries) < maxEntries {
		recordWithCap(r, "llm_in", fmt.Sprintf("第 %d 轮输入\n%s", round, logx.Truncate(inputJSON, llmTextCap)), in, 0, ms)
	}
}

// RecordLLMMessage 记录模型本轮的原始输出（文本 + 工具调用意图 JSON），
// 并带上刚记录的 llm_in 暂存的 token 用量。
func RecordLLMMessage(ctx context.Context, outJSON string) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	if len(r.Entries) < maxEntries {
		recordWithCap(r, "llm_out", logx.Truncate(outJSON, llmTextCap), r.pendIn, r.pendOut, r.pendMs)
	}
	r.pendIn, r.pendOut, r.pendMs = 0, 0, 0
}

// End 登记调用结束：盖时间戳；errMsg 非空时补记错误。
func End(id, errMsg string) {
	mu.Lock()
	r, ok := reqs[id]
	mu.Unlock()
	if !ok {
		return
	}
	if errMsg != "" {
		Record(logx.WithID(context.Background(), id), "error", "%s", errMsg)
	}
	mu.Lock()
	r.Ended = time.Now()
	mu.Unlock()
}

// List 返回全部轨迹摘要（不含逐条事件），最新的在前。
func List() []Request {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Request, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		r := reqs[order[i]]
		if r == nil {
			continue
		}
		cp := *r
		cp.Entries = nil
		out = append(out, cp)
	}
	return out
}

// Get 返回一条完整轨迹（含逐条事件）。
func Get(id string) (Request, bool) {
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return Request{}, false
	}
	return *r, true
}
