package eventbus

import (
	"sync"
	"testing"
	"time"
)

// TestPublishAndReceive 发布的事件要能被订阅者收到，seq 从 1 起单调递增。
func TestPublishAndReceive(t *testing.T) {
	b := New(0, 0)
	sub, _, needResync := b.Subscribe("web", 0)
	if needResync {
		t.Fatal("首次订阅不应要求重拉")
	}
	defer sub.Close()

	seq := b.Publish("web", TypeDelta, "t1", Origin{ClientID: "c1", Device: "手机端"},
		map[string]any{"delta": "你好"})
	if seq != 1 {
		t.Fatalf("首个事件 seq = %d，期望 1", seq)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != TypeDelta || ev.TurnID != "t1" {
			t.Fatalf("事件不对：%+v", ev)
		}
		if ev.ClientID != "c1" || ev.Device != "手机端" {
			t.Fatalf("来源没带上：%+v", ev)
		}
		if ev.Seq != 1 {
			t.Fatalf("事件 seq = %d，期望 1", ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("没收到事件")
	}
}

// TestSessionIsolation 不同会话互不串扰：给 acp:1 发的事件不该出现在 web 的订阅里。
func TestSessionIsolation(t *testing.T) {
	b := New(0, 0)
	web, _, _ := b.Subscribe("web", 0)
	defer web.Close()
	acp, _, _ := b.Subscribe("acp:1", 0)
	defer acp.Close()

	b.Publish("acp:1", TypeDelta, "t1", Origin{}, map[string]any{"delta": "只给 ACP"})

	select {
	case ev := <-web.Events():
		t.Fatalf("web 不该收到 ACP 的事件：%+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case ev := <-acp.Events():
		if ev.Session != "acp:1" {
			t.Fatalf("会话不对：%+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("ACP 没收到事件")
	}
}

// TestReplaySince 断线重连要能补发 since 之后的事件（客户端靠它不漏消息）。
func TestReplaySince(t *testing.T) {
	b := New(0, 0)
	for i := 0; i < 3; i++ {
		b.Publish("web", TypeDelta, "t1", Origin{}, map[string]any{"delta": "x"})
	}
	sub, backlog, needResync := b.Subscribe("web", 1)
	defer sub.Close()
	if needResync {
		t.Fatal("缓冲内的缺口不该要求重拉")
	}
	if backlog != 2 {
		t.Fatalf("补发条数 = %d，期望 2", backlog)
	}
	ch := sub.Events()
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			if ev.Seq != int64(i+2) {
				t.Fatalf("补发事件 seq = %d，期望 %d", ev.Seq, i+2)
			}
		case <-time.After(time.Second):
			t.Fatal("补发超时")
		}
	}
}

// TestNeedResyncWhenTooFarBehind 客户端落后到缓冲之外时要明说「请全量重拉」，
// 而不是假装事件是连续的。
func TestNeedResyncWhenTooFarBehind(t *testing.T) {
	b := New(2, 0) // 缓冲只留 2 条
	for i := 0; i < 5; i++ {
		b.Publish("web", TypeDelta, "t1", Origin{}, map[string]any{"delta": "x"})
	}
	sub, _, needResync := b.Subscribe("web", 1)
	defer sub.Close()
	if !needResync {
		t.Fatal("落后到缓冲外时应要求重拉")
	}
}

// TestCurrentSeq 提交接口靠它回给前端订阅游标。
func TestCurrentSeq(t *testing.T) {
	b := New(0, 0)
	if b.CurrentSeq("web") != 0 {
		t.Fatal("空会话 seq 应为 0")
	}
	b.Publish("web", TypeDelta, "t1", Origin{}, map[string]any{"delta": "x"})
	if b.CurrentSeq("web") != 1 {
		t.Fatalf("seq = %d，期望 1", b.CurrentSeq("web"))
	}
}

// TestPublishNeverBlocks 慢订阅者不能拖住发布方（对话主链路最怕被卡住）。
func TestPublishNeverBlocks(t *testing.T) {
	b := New(0, 1) // 单连接缓冲只留 1 条
	sub, _, _ := b.Subscribe("web", 0)
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish("web", TypeDelta, "t1", Origin{}, map[string]any{"delta": "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("发布被慢订阅者卡住了")
	}
}

// TestConcurrentPublishSubscribe 并发发布与订阅不许有竞态（-race 才看得出来）。
func TestConcurrentPublishSubscribe(t *testing.T) {
	b := New(0, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Publish("web", TypeDelta, "t1", Origin{}, map[string]any{"delta": "x"})
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, _, _ := b.Subscribe("web", 0)
			time.Sleep(10 * time.Millisecond)
			sub.Close()
		}()
	}
	wg.Wait()
}
