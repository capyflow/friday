package llmx

import (
	"context"
	"testing"

	"github.com/capyflow/friday/internal/usage"
	"github.com/capyflow/vortexagent/llm"
)

// stubProvider 记录收到的请求，返回固定用量。
type stubProvider struct {
	lastReq *llm.ChatRequest
	usage   llm.Usage
	err     error
}

func (s *stubProvider) Name() string       { return "stub" }
func (s *stubProvider) ContextWindow() int { return 32000 }

func (s *stubProvider) Chat(_ context.Context, req *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &llm.ChatResponse{
		Message: llm.NewTextMessage(llm.RoleAssistant, "摘要"),
		Usage:   s.usage,
	}, nil
}

// TestCountedRecordsAttribution 摘要器的调用绕过 agent Hooks，
// 装饰层必须把它计入账本并标上来源，否则长会话归档成本成为账本暗区。
func TestCountedRecordsAttribution(t *testing.T) {
	usage.Init("")
	base := &stubProvider{usage: llm.Usage{InputTokens: 900, OutputTokens: 120, CacheReadTokens: 300}}
	c := &Counted{Base: base, Origin: "summarizer", Session: "web", Model: "fallback-model"}

	if _, err := c.Chat(context.Background(), &llm.ChatRequest{Model: "qwen-max"}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	list, total := usage.Summary()
	if total.Calls != 1 || total.In != 900 || total.Out != 120 {
		t.Fatalf("记账不符: %+v", total)
	}
	if list[0].ByOrigin["summarizer"] == nil {
		t.Errorf("应按 summarizer 来源分桶: %+v", list[0].ByOrigin)
	}
	if list[0].ByModel["qwen-max"] == nil {
		t.Errorf("应记请求模型而非兜底名: %+v", list[0].ByModel)
	}
	if list[0].ByModel["qwen-max"].CacheRead != 300 {
		t.Errorf("缓存读 token 未累计: %+v", list[0].ByModel["qwen-max"])
	}
}

// TestCountedFallsBackToConfiguredModel 请求未带模型名时用构造时的模型兜底。
func TestCountedFallsBackToConfiguredModel(t *testing.T) {
	usage.Init("")
	c := &Counted{Base: &stubProvider{usage: llm.Usage{InputTokens: 1, OutputTokens: 1}}, Model: "deepseek-chat"}
	if _, err := c.Chat(context.Background(), &llm.ChatRequest{}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	list, _ := usage.Summary()
	if list[0].ByModel["deepseek-chat"] == nil {
		t.Errorf("应使用兜底模型名: %+v", list[0].ByModel)
	}
}

// TestCountedRecordsFailure 请求失败也要计一次调用（零 token），
// 否则重试风暴在账本上完全隐形。
func TestCountedRecordsFailure(t *testing.T) {
	usage.Init("")
	c := &Counted{Base: &stubProvider{err: context.Canceled}, Origin: "summarizer", Model: "m"}
	if _, err := c.Chat(context.Background(), &llm.ChatRequest{Model: "m"}, nil); err == nil {
		t.Fatal("期望透传错误")
	}
	_, total := usage.Summary()
	if total.Calls != 1 || total.In != 0 {
		t.Errorf("失败调用应计一次零用量: %+v", total)
	}
}

// TestCountedPassesThroughProviderMeta 装饰层不得改变 provider 元信息。
func TestCountedPassesThroughProviderMeta(t *testing.T) {
	c := &Counted{Base: &stubProvider{}}
	if c.Name() != "stub" || c.ContextWindow() != 32000 {
		t.Errorf("元信息未透传: name=%q window=%d", c.Name(), c.ContextWindow())
	}
}
