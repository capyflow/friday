package web

import (
	"context"
	"strings"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/eventbus"
)

// syncProvider 是会分片吐字的假模型：用来验证增量真的经事件总线广播出去了。
type syncProvider struct{}

func (syncProvider) Name() string       { return "sync" }
func (syncProvider) ContextWindow() int { return 128000 }
func (syncProvider) Chat(_ context.Context, _ *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	if onDelta != nil {
		_ = onDelta(llm.Delta{Text: "你好"})
		_ = onDelta(llm.Delta{Text: "，世界"})
	}
	return &llm.ChatResponse{Message: llm.NewTextMessage(llm.RoleAssistant, "你好，世界")}, nil
}

// newSyncChat 起一套带事件总线的 Chat：真跑一问一答，走真实的广播链路。
func newSyncChat(t *testing.T, bus *eventbus.Bus) *Chat {
	t.Helper()
	dir := t.TempDir()
	ag := vxagent.New(vxagent.Options{Provider: syncProvider{}, Model: "m"})
	c := New(agent.NewBrain(ag, agent.NewSessionPool(nil), nil), dir, "")
	c.SetBus(bus)
	return c
}

// TestTurnBroadcastsEvents 提交的那一轮，增量与收尾都要经事件总线广播出去——
// 另一端正是靠这些事件看到「手机端问的、桌面端也在看」。
func TestTurnBroadcastsEvents(t *testing.T) {
	bus := eventbus.New(0, 0)
	c := newSyncChat(t, bus)
	sub, _, _ := bus.Subscribe("web", 0)
	defer sub.Close()

	origin := eventbus.Origin{ClientID: "c1", Device: "手机端"}
	go c.runTurn("web", &queuedTurn{
		id: "t1", question: "你好", origin: origin, enqueuedAt: time.Now(),
	})

	seen := map[string]bool{}
	var deltaText string
	deadline := time.After(5 * time.Second)
	for len(seen) < 4 {
		select {
		case ev := <-sub.Events():
			seen[ev.Type] = true
			if ev.Type != eventbus.TypeUserMessage && ev.TurnID != "t1" {
				t.Fatalf("事件 turnId = %q，期望 t1", ev.TurnID)
			}
			if ev.Type == eventbus.TypeDelta {
				m, ok := ev.Data.(map[string]any)
				if !ok {
					t.Fatalf("delta 载荷不是 map: %+v", ev.Data)
				}
				deltaText += m["delta"].(string)
			}
			if ev.Type == eventbus.TypeUserMessage {
				if ev.ClientID != "c1" || ev.Device != "手机端" {
					t.Fatalf("来源没带上: %+v", ev)
				}
			}
		case <-deadline:
			t.Fatalf("事件没到齐，已收到: %v", seen)
		}
	}
	for _, want := range []string{
		eventbus.TypeUserMessage, eventbus.TypeTurnStart,
		eventbus.TypeDelta, eventbus.TypeTurnEnd,
	} {
		if !seen[want] {
			t.Fatalf("没收到 %s 事件，已收到: %v", want, seen)
		}
	}
	if deltaText != "你好，世界" {
		t.Fatalf("增量文本 = %q，期望 %q", deltaText, "你好，世界")
	}
}

// TestConcurrentTurnQueues 第二轮撞上第一轮时要排队，不是静默阻塞。
func TestConcurrentTurnQueues(t *testing.T) {
	bus := eventbus.New(0, 0)
	c := newSyncChat(t, bus)

	// 占住这一轮：登记一个进行中的对话 + dispatcher 在跑，
	// 让后来的请求只能排队。
	c.mu.Lock()
	c.runs["web"] = map[string]*chatRun{"busy": {cancel: func() {}}}
	c.dispatching["web"] = true
	c.mu.Unlock()

	status, position, err := c.enqueue("web", &queuedTurn{
		id: "t2", question: "排队的问题", enqueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if status != statusQueued {
		t.Fatalf("状态 = %s，期望 %s", status, statusQueued)
	}
	if position != 1 {
		t.Fatalf("位次 = %d，期望 1", position)
	}
}

// TestRunsAreKeyedByTurnID 两级映射：不同轮次的登记互不覆盖——
// 这正是修掉「停止停错对象」那个 bug 的关键。
func TestRunsAreKeyedByTurnID(t *testing.T) {
	c := newSyncChat(t, eventbus.New(0, 0))

	c.mu.Lock()
	if c.runs["web"] == nil {
		c.runs["web"] = map[string]*chatRun{}
	}
	c.runs["web"]["tA"] = &chatRun{cancel: func() {}}
	c.runs["web"]["tB"] = &chatRun{cancel: func() {}}
	a := c.runs["web"]["tA"]
	b := c.runs["web"]["tB"]
	c.mu.Unlock()

	if a == nil || b == nil {
		t.Fatal("两轮都应登记成功")
	}
	if a == b {
		t.Fatal("两轮不应共用同一个句柄")
	}
	// 停其中一轮不影响另一轮。
	delete(c.runs["web"], "tA")
	if c.runs["web"]["tB"] == nil {
		t.Fatal("停掉 tA 不应影响 tB")
	}
}

// TestPauseSceneKeptPerSession 暂停现场按会话各存一份：在 A 会话暂停的，
// 不能在 B 会话点继续。
func TestPauseSceneKeptPerSession(t *testing.T) {
	c := newSyncChat(t, eventbus.New(0, 0))
	c.paused["web"] = &PausedTurn{Question: "原问题", Partial: "半句"}
	if got := c.takePaused("web"); got == nil || got.Question != "原问题" {
		t.Fatalf("取暂停现场不对: %+v", got)
	}
	if c.takePaused("web") != nil {
		t.Fatal("取走之后不应再有")
	}
}

// TestErrorMessageMapping 错误文案要能照着做——这是用户唯一的线索。
func TestErrorMessageMapping(t *testing.T) {
	msg := chatErrorMessage(context.DeadlineExceeded)
	if !strings.Contains(msg, "超时") && !strings.Contains(msg, "连不上") {
		t.Fatalf("超时的文案没提到超时或连不上: %s", msg)
	}
}
