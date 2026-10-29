package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/logx"
)

// fakeProvider 假 LLM：按序返回预置回答；block 非空时 Chat 阻塞直至关闭或
// ctx 取消（模拟生成中被打断，测试经 callsCount 等待进入阻塞）。
type fakeProvider struct {
	mu        sync.Mutex
	responses []string
	calls     int
	block     chan struct{}
	lastMsgs  []llm.Message // 最近一次请求带的 messages（验证注入用）
}

// messages 返回最近一次请求的 messages 副本。
func (f *fakeProvider) messages() []llm.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Message{}, f.lastMsgs...)
}

func (f *fakeProvider) Name() string       { return "fake" }
func (f *fakeProvider) ContextWindow() int { return 128000 }
func (f *fakeProvider) callsCount() int    { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fakeProvider) responseText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls <= 0 || f.calls > len(f.responses) {
		return "回答"
	}
	return f.responses[f.calls-1]
}

func (f *fakeProvider) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.calls++
	if req != nil {
		f.lastMsgs = append([]llm.Message{}, req.Messages...)
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	text := f.responseText()
	if onDelta != nil {
		_ = onDelta(llm.Delta{Text: text})
	}
	return &llm.ChatResponse{Message: llm.NewTextMessage(llm.RoleAssistant, text)}, nil
}

// TestEventsHub 过程事件按请求 ID 路由、退订后不再收到。
func TestEventsHub(t *testing.T) {
	ch, stop := SubscribeSteps("req-1")
	defer stop()
	other, stopOther := SubscribeSteps("req-2")
	defer stopOther()

	ctx1 := logx.WithID(context.Background(), "req-1")
	PublishStep(ctx1, Step{Kind: "tool", Name: "exec_command", Status: "run", Detail: "uname"})
	select {
	case s := <-ch:
		if s.Name != "exec_command" || s.Status != "run" {
			t.Fatalf("事件内容不符: %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("应收到 req-1 的事件")
	}
	select {
	case <-other:
		t.Fatal("req-2 不应收到 req-1 的事件")
	default:
	}

	// 退订后不再投递。
	stop()
	PublishStep(ctx1, Step{Kind: "round", Detail: "第 1 轮"})
	select {
	case s := <-ch:
		t.Fatalf("退订后不应收到事件: %+v", s)
	default:
	}

	// 无请求 ID 的 ctx 发布被安全忽略。
	PublishStep(context.Background(), Step{Kind: "round"})
}

// newTestBrain 用假 Provider 组装 Brain（内存会话池）。
func newTestBrain(t *testing.T, p llm.Provider) *Brain {
	t.Helper()
	reg := vxagent.NewRegistry()
	ag := vxagent.New(vxagent.Options{Provider: p, Registry: reg})
	return NewBrain(ag, NewSessionPool(nil), nil)
}

// TestBrainResume 验收暂停/继续链路：取消中的对话回滚会话；Resume 把原问题与
// 部分回答重新注入会话，模型接着完成（同一会话上下文）。
func TestBrainResume(t *testing.T) {
	p := &fakeProvider{responses: []string{"回答A", "部分B", "继续B"}}
	brain := newTestBrain(t, p)
	ctx := logx.WithID(context.Background(), "resume-test")
	ctx = WithSessionKey(ctx, SessionWeb)

	// 1) 正常一问一答，会话有 2 条消息。
	if _, err := brain.Ask(ctx, "问题A", nil, nil); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	msgs := brain.Pool().History(ctx, SessionWeb)
	if len(msgs) != 2 {
		t.Fatalf("首轮后应有 2 条消息，实际 %d", len(msgs))
	}

	// 2) 第二问生成中取消 ctx（模拟暂停）：框架回滚本轮，会话仍是 2 条。
	p.mu.Lock()
	p.block = make(chan struct{})
	p.mu.Unlock()
	askCtx, askCancel := context.WithCancel(ctx)
	defer askCancel()
	askDone := make(chan error, 1)
	go func() {
		_, err := brain.Ask(askCtx, "问题B", nil, nil)
		askDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for p.callsCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // 确保 Chat 已阻塞在 select 上
	askCancel()
	if err := <-askDone; err == nil {
		t.Fatal("取消后 Ask 应返回错误")
	}
	msgs = brain.Pool().History(ctx, SessionWeb)
	if len(msgs) != 2 {
		t.Fatalf("暂停回滚后应仍是 2 条消息，实际 %d", len(msgs))
	}

	// 3) Resume：注入问题B + 部分回答B，继续完成。
	p.mu.Lock()
	p.block = nil
	p.mu.Unlock()
	answer, err := brain.Resume(ctx, SessionWeb, "问题B", "部分B", nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if answer != "继续B" {
		t.Fatalf("Resume 回答应为继续B，实际 %q", answer)
	}
	msgs = brain.Pool().History(ctx, SessionWeb)
	// 期望：问题A + 回答A + 问题B + 部分B(+暂停标记) + 继续指令 + 回答B
	if len(msgs) != 6 {
		t.Fatalf("Resume 后应有 6 条消息，实际 %d", len(msgs))
	}
	joined := ""
	for _, m := range msgs {
		for _, c := range m.Content {
			joined += c.Text + "\n"
		}
	}
	for _, want := range []string{"问题A", "回答A", "问题B", "部分B", "暂停"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("会话历史应包含 %q，实际:\n%s", want, joined)
		}
	}
}
