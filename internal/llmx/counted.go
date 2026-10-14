// Package llmx 是 LLM provider 的装饰层：把 Friday 侧的 token 记账挂到框架
// provider 上，不改变请求语义。
package llmx

import (
	"context"

	"github.com/capyflow/friday/internal/usage"
	"github.com/capyflow/vortexagent/llm"
)

// Counted 包装一个 Provider，把每次调用计入 token 账本。
//
// 存在的理由：上下文引擎的摘要器直接调用 provider.Chat（vortexagent 的
// ctxengine/archiver.go），不经过 agent Hooks——这部分 token 否则完全不入账，
// 长会话的归档成本就成了账本上的暗区。
type Counted struct {
	Base    llm.Provider
	Origin  string // 记账来源，如 "summarizer"
	Session string // 归属会话 key，可空
	Model   string // 请求未带模型名时的兜底
}

var _ llm.Provider = (*Counted)(nil)

func (c *Counted) Name() string       { return c.Base.Name() }
func (c *Counted) ContextWindow() int { return c.Base.ContextWindow() }

func (c *Counted) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	resp, err := c.Base.Chat(ctx, req, onDelta)

	model := c.Model
	if req != nil && req.Model != "" {
		model = req.Model
	}
	call := usage.Call{Model: model, Origin: c.Origin, Session: c.Session}
	if resp != nil {
		call.In = int64(resp.Usage.InputTokens)
		call.Out = int64(resp.Usage.OutputTokens)
		call.CacheRead = int64(resp.Usage.CacheReadTokens)
		call.CacheWrite = int64(resp.Usage.CacheWriteTokens)
	}
	usage.Record(call)
	return resp, err
}
